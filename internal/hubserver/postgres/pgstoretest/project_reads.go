package pgstoretest

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ProjectReadCounts struct {
	tenantstore.Repository
	Transactions int
	Projects     int
	Models       int
	Active       int
}

func (r *ProjectReadCounts) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.Transactions++
	r.Active++
	defer func() { r.Active-- }()
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error { return read(countedProjectReader{Reader: reader, owner: r}) })
}

type countedProjectReader struct {
	tenantstore.Reader
	owner *ProjectReadCounts
}

func (r countedProjectReader) ProjectRecord(ctx context.Context, project tracker.ProjectID) (projectstore.Record, error) {
	r.owner.Projects++
	return r.Reader.ProjectRecord(ctx, project)
}

func (r countedProjectReader) IntegrationPage(ctx context.Context, project tracker.ProjectID) (projectstore.Integration, error) {
	reader, ok := r.Reader.(projectstore.IntegrationPageReader)
	if !ok {
		return projectstore.Integration{}, errors.New("integration page reader unavailable")
	}
	return reader.IntegrationPage(ctx, project)
}

func (r countedProjectReader) OrganizationModels(ctx context.Context) (policystore.OrganizationModels, error) {
	r.owner.Models++
	return r.Reader.OrganizationModels(ctx)
}
