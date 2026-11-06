package hubserver

import (
	"context"
	"sync/atomic"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryKeyReadCountingRepository struct {
	tenantstore.Repository
	readCalls int
}

type entryKeyReadCounterContextKey struct{}

type entryKeyReadCounter struct {
	projectAccessPageReads atomic.Int64
}

type entryKeyReadCountingReader struct {
	tenantstore.Reader
	accessstore.ProjectAccessPageReader
}

func (r entryKeyReadCountingReader) ProjectAccessPage(ctx context.Context, filter accessstore.ProjectAccessFilter) ([]accessstore.ProjectAccess, error) {
	if counter, ok := ctx.Value(entryKeyReadCounterContextKey{}).(*entryKeyReadCounter); ok {
		counter.projectAccessPageReads.Add(1)
	}
	return r.ProjectAccessPageReader.ProjectAccessPage(ctx, filter)
}

func (r *entryKeyReadCountingRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.readCalls++
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		pageReader, ok := reader.(accessstore.ProjectAccessPageReader)
		if !ok {
			return read(reader)
		}
		return read(entryKeyReadCountingReader{Reader: reader, ProjectAccessPageReader: pageReader})
	})
}

func (r *entryKeyReadCountingRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}
