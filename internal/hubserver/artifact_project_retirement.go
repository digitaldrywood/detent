package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) pruneProjectRetirementArtifacts(ctx context.Context, scope nativeScope, current projectstore.Record, request deleteCloudProjectRequest, operation, key, hash string) (bool, *eventstore.Receipt, error) {
	var ready bool
	var previous *eventstore.Receipt
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		receipt, err := w.Receipt(ctx, scope.credential.ID, operation, key)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
			}
			previous = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := s.requireProjectRetirementEntitlement(ctx, w, s.config.now()); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		fresh, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if fresh.Settings.Revision != current.Settings.Revision || fresh.Core.ContentSettingsRevision != current.Core.ContentSettingsRevision || fresh.Core.Name != request.ConfirmName {
			return corestore.ErrRevisionConflict
		}
		ready, err = w.PruneProjectArtifactBindings(ctx, scope.project, artifactstore.ProjectRetirementBatch)
		return err
	})
	return ready, previous, err
}
