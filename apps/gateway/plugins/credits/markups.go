package credits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maximhq/bifrost/core/schemas"
)

// Staff set the markups and the USD to SAR rate in the admin console; the plugin reads them
// from the control plane database (docs/platform/adr/0010-admin-controlled-pricing.md,
// 0017-sar-ledger.md).

const (
	markupRefreshInterval = 30 * time.Second
	// markupRetryInterval is used until the first load succeeds: the plugin refuses billable
	// requests while it has no markups, so it should not wait a full interval to recover.
	markupRetryInterval = 2 * time.Second
	markupBootTimeout   = 3 * time.Second
)

// markups is one snapshot of the prices staff set: the markups and the USD to SAR rate. It is
// never modified once published.
type markups struct {
	defaultBps int64
	byProvider map[string]int64
	// fx is the official rate, the bank's spread and the effective rate a new hold converts at
	fx fxRate
}

// forProvider returns the markup for the provider of an attempt, or the default.
func (m *markups) forProvider(provider schemas.ModelProvider) int64 {
	if bps, ok := m.byProvider[string(provider)]; ok {
		return bps
	}
	return m.defaultBps
}

// pricingSource loads the current markups.
type pricingSource interface {
	LoadMarkups(ctx context.Context) (*markups, error)
}

const loadMarkupsSQL = `
SELECT s.default_markup_bps,
       s.usd_to_sar_rate_e6,
       s.fx_spread_bps,
       coalesce((SELECT jsonb_object_agg(provider, markup_bps) FROM provider_markup), '{}'::jsonb)
  FROM pricing_settings s
 WHERE s.id = 1`

// LoadMarkups reads the default markup, the USD to SAR rate and spread, and the per-provider
// markups in one statement, so the snapshot is consistent.
func (s *Store) LoadMarkups(ctx context.Context) (*markups, error) {
	m := &markups{}
	var rateE6, spreadBps int64
	err := s.pool.QueryRow(ctx, loadMarkupsSQL).Scan(&m.defaultBps, &rateE6, &spreadBps, &m.byProvider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("credits: pricing_settings has no row")
	}
	if err != nil {
		return nil, fmt.Errorf("credits: load markups: %w", err)
	}
	// the database enforces the bounds; a negative value here would mean it does not
	if m.defaultBps < 0 {
		return nil, fmt.Errorf("credits: negative default markup %d", m.defaultBps)
	}
	if m.fx, err = newFXRate(rateE6, spreadBps); err != nil {
		return nil, err
	}
	for provider, bps := range m.byProvider {
		if bps < 0 {
			return nil, fmt.Errorf("credits: negative markup %d for %s", bps, provider)
		}
	}
	return m, nil
}

// refreshMarkups loads the markups and publishes them. On failure the last snapshot stays.
func (p *Plugin) refreshMarkups(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ledgerTimeout)
	defer cancel()
	m, err := p.prices.LoadMarkups(ctx)
	if err != nil {
		if p.markups.Load() == nil {
			p.logger.Error("credits: markups could not be loaded, billable requests are refused until they are: %v", err)
		} else {
			p.logger.Warn("credits: markups could not be reloaded, the last ones loaded stay in use: %v", err)
		}
		return err
	}
	p.markups.Store(m)
	return nil
}

// refreshLoop reloads the markups until ctx ends: every markupRefreshInterval, or every
// markupRetryInterval while none has ever been loaded.
func (p *Plugin) refreshLoop(ctx context.Context, interval, retry time.Duration) {
	timer := time.NewTimer(p.nextRefresh(interval, retry))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = p.refreshMarkups(ctx)
			timer.Reset(p.nextRefresh(interval, retry))
		}
	}
}

func (p *Plugin) nextRefresh(interval, retry time.Duration) time.Duration {
	if p.markups.Load() == nil {
		return retry
	}
	return interval
}
