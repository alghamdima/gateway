package credits

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The gateway generates its own customer ids, so the control plane keeps a table from
// organization to gateway customer (docs/platform/adr/0004-key-model.md). The governance
// plugin puts the customer id on the context; this turns it into the organization whose
// balance pays.

const organizationForSQL = `SELECT organization_id FROM gateway_customer WHERE customer_id = $1`

// OrganizationFor returns the organization a gateway customer belongs to. ok is false when
// the customer is not mapped to any organization.
func (s *Store) OrganizationFor(ctx context.Context, customerID string) (organizationID string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, organizationForSQL, customerID).Scan(&organizationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("credits: organization for customer: %w", err)
	}
	return organizationID, true, nil
}

// organizationFor resolves a customer through an in-memory cache. Only hits are cached: a
// mapping never changes once made, and an organization that is deleted takes its credit
// account with it, so a stale hit is refused by the reservation. A miss is asked again next
// time, so a newly created organization works at once.
func (p *Plugin) organizationFor(ctx context.Context, customerID string) (string, bool, error) {
	if cached, ok := p.organizations.Load(customerID); ok {
		return cached.(string), true, nil
	}
	organizationID, ok, err := p.ledger.OrganizationFor(ctx, customerID)
	if err != nil || !ok {
		return "", ok, err
	}
	p.organizations.Store(customerID, organizationID)
	return organizationID, true, nil
}
