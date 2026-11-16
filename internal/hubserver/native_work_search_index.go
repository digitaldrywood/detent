package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) searchRefreshTimer() hubdeps.Timer {
	return s.config.deps.NewMaintenanceTimer(searchRefreshOffset(s.config.WebhookMaintenanceInterval, s.config.deps.Jitter()))
}

func searchRefreshOffset(interval time.Duration, jitter float64) time.Duration {
	return time.Duration(min(max(jitter, 0), 1) * float64(interval-1))
}

func (s *Service) searchContent() *searchstore.Content {
	return searchstore.NewContentWithCursors(s.runDetailStore(), searchstore.NewCursorSealer(s.readNativeCursorKey))
}

func (s *Service) maintainNativeSearch(ctx context.Context) {
	base := s.runDetailContext(ctx)
	identity, cancelIdentity := s.config.deps.SearchRefreshTimeout(base, searchstore.RefreshTickDuration)
	if _, err := s.refreshNativeIdentity(identity); err != nil && identity.Err() == nil {
		s.config.Logger.ErrorContext(ctx, "rebuild scoped origin identities", "error", err)
	}
	cancelIdentity()
	_, err := s.searchContent().RefreshTick(base, tenantstore.SearchRepository{Repository: s.config.TenantRepository}, s.accessOrganization(), s.config.deps.SearchRefreshTimeout)
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
