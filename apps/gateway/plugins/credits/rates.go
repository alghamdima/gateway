package credits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Staff set the USD to SAR rate and the bank's spread in the admin console; the plugin reads
// them from the control plane database (docs/platform/adr/0010-admin-controlled-pricing.md,
// 0017-sar-ledger.md). A request is charged its provider's cost at the effective rate, with
// no markup: the platform's revenue is the fee taken at top-up.

const (
	rateRefreshInterval = 30 * time.Second
	// rateRetryInterval is used until the first load succeeds: the plugin refuses billable
	// requests while it has no rate, so it should not wait a full interval to recover.
	rateRetryInterval = 2 * time.Second
	rateBootTimeout   = 3 * time.Second
)

// rateSource loads the current USD to SAR rate.
type rateSource interface {
	LoadRate(ctx context.Context) (fxRate, error)
}

const loadRateSQL = `
SELECT usd_to_sar_rate_e6, fx_spread_bps
  FROM pricing_settings
 WHERE id = 1`

// LoadRate reads the official USD to SAR rate and the bank's spread in one statement, so the
// effective rate is worked out from one consistent pair.
func (s *Store) LoadRate(ctx context.Context) (fxRate, error) {
	var rateE6, spreadBps int64
	err := s.pool.QueryRow(ctx, loadRateSQL).Scan(&rateE6, &spreadBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return fxRate{}, errors.New("credits: pricing_settings has no row")
	}
	if err != nil {
		return fxRate{}, fmt.Errorf("credits: load rate: %w", err)
	}
	// the database enforces the bounds; newFXRate refuses a value that got past them
	return newFXRate(rateE6, spreadBps)
}

// refreshRate loads the rate and publishes it. On failure the last one loaded stays.
func (p *Plugin) refreshRate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, ledgerTimeout)
	defer cancel()
	fx, err := p.rates.LoadRate(ctx)
	if err != nil {
		if p.rate.Load() == nil {
			p.logger.Error("credits: the USD to SAR rate could not be loaded, billable requests are refused until it is: %v", err)
		} else {
			p.logger.Warn("credits: the USD to SAR rate could not be reloaded, the last one loaded stays in use: %v", err)
		}
		return err
	}
	p.rate.Store(&fx)
	return nil
}

// refreshLoop reloads the rate until ctx ends: every rateRefreshInterval, or every
// rateRetryInterval while none has ever been loaded.
func (p *Plugin) refreshLoop(ctx context.Context, interval, retry time.Duration) {
	timer := time.NewTimer(p.nextRefresh(interval, retry))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = p.refreshRate(ctx)
			timer.Reset(p.nextRefresh(interval, retry))
		}
	}
}

func (p *Plugin) nextRefresh(interval, retry time.Duration) time.Duration {
	if p.rate.Load() == nil {
		return retry
	}
	return interval
}
