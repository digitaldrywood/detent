package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeSearchBytePrefix(content string, budget int) string {
	if len(content) <= budget {
		return content
	}
	for budget > 0 && !utf8.RuneStart(content[budget]) {
		budget--
	}
	return content[:budget]
}

type tenantSearchRepository struct {
	tenantstore.Repository
}

func (r tenantSearchRepository) ReadSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(searchstore.Reader) error) error {
	return r.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error { return read(reader) })
}

func (r tenantSearchRepository) WriteSearch(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(searchstore.Writer) error) error {
	return r.WriteTenant(ctx, organization, workload, func(writer tenantstore.Writer) error { return write(writer) })
}

func (s *Service) maintainNativeSearch(ctx context.Context) {
	if _, err := s.refreshNativeIdentity(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "rebuild scoped origin identities", "error", err)
	}
	_, err := searchstore.NewContent(s.runDetailStore()).RefreshOne(s.runDetailContext(ctx), tenantSearchRepository{s.config.TenantRepository}, s.accessOrganization())
	if err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "rebuild scoped search index", "error", err)
	}
}

func (s *Service) refreshNativeIdentity(ctx context.Context) (bool, error) {
	var head postgres.DoctorProposalIdentityHead
	organization := s.accessOrganization()
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		reader, ok := r.(postgres.DoctorProposalIdentityReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		head, err = reader.NextDoctorProposalIdentityHead(ctx)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	issue, err := s.hydrateCanonicalWorkItemRecord(s.runDetailContext(ctx), head.Item, head.Version)
	if err != nil {
		return true, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		return w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: organization, ProjectID: head.Item.ProjectID, WorkItemID: head.Item.NativeID, Revision: head.Version.Revision, VersionSHA256: head.Version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(issue.Body)})
	})
	if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrNotFound) {
		return true, nil
	}
	return true, err
}
