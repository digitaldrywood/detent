package pgcore

import (
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Transaction struct {
	Organization tracker.OrganizationID
	DB           pgquery.DBTX
	Queries      *pgquery.Queries
}

func (t Transaction) CheckOrganization(organization tracker.OrganizationID) error {
	if organization != t.Organization {
		return corestore.ErrTenantMismatch
	}
	return nil
}
