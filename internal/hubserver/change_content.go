package hubserver

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) contentContext(ctx context.Context) context.Context {
	if ctx.Value(hubcontent.ChangeContentKey{}) != nil {
		return ctx
	}
	call := s.storageArchiveContent()
	store := s.runDetailStore()
	if call == nil && store == nil {
		return ctx
	}
	organization := s.accessOrganization()
	readReference := func(ctx context.Context, key string) (attachment.ArchiveReference, error) {
		if organization == "" || s.config.TenantRepository == nil {
			return attachment.ArchiveReference{}, hubcontent.ErrContentStorageUnconfigured
		}
		var ref attachment.ArchiveReference
		err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(hubrecords.AttemptArchiveReader)
			if !ok {
				return hubcontent.ErrContentStorageUnconfigured
			}
			object, err := reader.ArchiveObjectByKey(ctx, key)
			if err != nil {
				return err
			}
			ref = object.Reference
			if object.OrganizationID != organization || ref.Key != key || ref.Validate(string(organization)) != nil || ref.CustomerOwned || ref.BindingID != "" || ref.SchemaVersion != 1 {
				return artifact.ErrIntegrity
			}
			return nil
		})
		return ref, err
	}
	return context.WithValue(ctx, hubcontent.ChangeContentKey{}, &hubcontent.ChangeContent{Encoder: &s.archiveEncoder, Call: call, Store: store, Register: s.registerChangeContent, ReadOrganization: string(organization), ReadReference: readReference})
}

func (s *Service) changeContentMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(s.contentContext(c.Request().Context())))
		return next(c)
	}
}

func prepareChangeContent(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	return prepareMeteredContent(ctx, organization, data, true)
}

func contentPreparationKey(organization string, data []byte, billable bool) string {
	key := organization + "/" + artifact.Digest(data)
	if !billable {
		key = "receipt/" + key
	}
	return key
}

func prepareMeteredContent(ctx context.Context, organization string, data []byte, billable bool) (attachment.ArchiveReference, error) {
	content, ok := ctx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent)
	if !ok {
		return attachment.ArchiveReference{}, hubcontent.ErrContentStorageUnconfigured
	}
	content.Mu.Lock()
	digest := contentPreparationKey(organization, data, billable)
	cached, found := content.Prepared[digest]
	content.Mu.Unlock()
	if found {
		return cached, nil
	}
	var ref attachment.ArchiveReference
	if content.Call == nil {
		var err error
		ref, err = content.Store.StoreDetail(ctx, organization, data)
		if err != nil {
			return ref, hubcontent.ImmutableContentStorageError(err)
		}
		if ref.Validate(organization) != nil || ref.UncompressedSize != int64(len(data)) {
			return ref, artifact.ErrIntegrity
		}
		verified, err := content.Store.ReadDetail(ctx, organization, ref)
		if err != nil {
			return ref, hubcontent.ImmutableContentStorageError(err)
		}
		if !bytes.Equal(data, verified) {
			return ref, artifact.ErrIntegrity
		}
		if err := content.Register(ctx, organization, ref, billable); err != nil {
			return ref, err
		}
	} else {
		encode := attachment.PrepareArchive
		if content.Encoder != nil {
			encode = content.Encoder.Prepare
		}
		prepared, compressed, err := encode(organization, 1, bytes.NewReader(data))
		if err != nil {
			return prepared, err
		}
		ref = prepared
		if err := content.Register(ctx, organization, ref, billable); err != nil {
			return ref, err
		}
		if _, err := content.Call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Data: compressed, Write: true}); err != nil {
			return ref, hubcontent.ImmutableContentStorageError(err)
		}
		verified, err := content.Call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
		if err != nil {
			return ref, hubcontent.ImmutableContentStorageError(err)
		}
		if !bytes.Equal(compressed, verified) {
			return ref, artifact.ErrIntegrity
		}
	}
	content.Mu.Lock()
	defer content.Mu.Unlock()
	if content.Prepared == nil {
		content.Prepared = make(map[string]attachment.ArchiveReference)
	}
	if len(content.Prepared) >= hubcontent.ClaimEvidenceLimit && content.MutationDepth == 0 {
		clear(content.Prepared)
	}
	if len(content.Prepared) >= hubcontent.ClaimEvidenceLimit {
		return ref, attachment.ErrTooLarge
	}
	content.Prepared[digest] = ref
	if err := content.RememberPayload(ref, data); err != nil {
		return ref, err
	}
	return ref, nil
}

func (s *Service) registerChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference, billable bool) error {
	return s.writeChangeContent(ctx, organization, ref, billable, true)
}

func (s *Service) registerConfigurationContent(ctx context.Context, organization string, ref attachment.ArchiveReference) error {
	return s.writeChangeContent(ctx, organization, ref, true, false)
}

func (s *Service) writeChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference, billable, withinAllowance bool) error {
	if err := ref.Validate(organization); err != nil {
		return err
	}
	if s.config.TenantRepository == nil {
		return hubapi.AttachmentAccountingUnavailable()
	}
	return s.config.TenantRepository.WriteTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		return s.registerChangeContentRecord(ctx, w, organization, ref, billable, withinAllowance)
	})
}

func changeContentCompletion(ctx context.Context) bool {
	if content, ok := ctx.Value(hubcontent.ChangeContentKey{}).(*hubcontent.ChangeContent); ok {
		content.Mu.Lock()
		defer content.Mu.Unlock()
		return content.Completion
	}
	return false
}

func (s *Service) registerChangeContentRecord(ctx context.Context, w tenantstore.Writer, organization string, ref attachment.ArchiveReference, billable, withinAllowance bool) error {
	if err := ref.Validate(organization); err != nil {
		return err
	}
	completion := changeContentCompletion(ctx)
	now := s.config.Clock().UTC()
	limit := int64(math.MaxInt64)
	plans := s.database.plansService().Plans()
	var entitlement hostedplans.HostedEntitlement
	if plans != nil {
		var err error
		entitlement, err = hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, w, now))
		if err != nil {
			return err
		}
		if allowance, ok := entitlement.Allowances["collaboration_bytes"]; ok && !completion && withinAllowance {
			limit = allowance
		}
	}
	if billable {
		if err := w.LockArchiveAccounting(ctx); err != nil {
			return err
		}
	}
	object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: tracker.OrganizationID(organization), ID: hubapi.NewID("archive"), Reference: ref, CreatedAt: now})
	if err != nil {
		return err
	}
	if billable {
		usage, err := w.ChargeArchiveObject(ctx, object.ID, limit)
		if errors.Is(err, corestore.ErrNotFound) {
			return hubapi.AttachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
	}
	if plans != nil && !completion && ctx.Value(runnerRequestBudgetContext{}) != true {
		window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
		used, err := w.BillingUsage(ctx, window, "api_mutations")
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if used >= entitlement.Allowances["api_mutations"] {
			return &hostedplans.LimitError{Resource: "api_mutations", Allowance: entitlement.Allowances["api_mutations"], Consumption: used}
		}
	}
	return nil
}

func (s *Service) checkChangeContentAllowance(ctx context.Context, organization string, refs map[string]attachment.ArchiveReference, next attachment.ArchiveReference) error {
	if s.config.TenantRepository == nil {
		return hubapi.AttachmentAccountingUnavailable()
	}
	plans := s.database.plansService().Plans()
	if plans == nil || changeContentCompletion(ctx) {
		return nil
	}
	now := s.config.Clock().UTC()
	return s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
		entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, r, now))
		if err != nil {
			return err
		}
		if limit, limited := entitlement.Allowances["collaboration_bytes"]; limited {
			used, err := r.CollaborationBytes(ctx)
			if errors.Is(err, corestore.ErrNotFound) {
				return hubapi.AttachmentAccountingUnavailable()
			}
			if err != nil {
				return err
			}
			pending := []attachment.ArchiveReference{next}
			for _, ref := range refs {
				if ref != next {
					pending = append(pending, ref)
				}
			}
			for _, ref := range pending {
				object, err := r.ArchiveObjectByReference(ctx, ref)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if ref.CustomerOwned || err == nil && object.CollaborationBytes != 0 {
					continue
				}
				if ref.CompressedSize > math.MaxInt64-used || used+ref.CompressedSize > limit {
					return &hostedplans.LimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
				}
				used += ref.CompressedSize
			}
		}
		if ctx.Value(runnerRequestBudgetContext{}) != true {
			window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
			used, err := r.BillingUsage(ctx, window, "api_mutations")
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if used >= entitlement.Allowances["api_mutations"] {
				return &hostedplans.LimitError{Resource: "api_mutations", Allowance: entitlement.Allowances["api_mutations"], Consumption: used}
			}
		}
		return nil
	})
}
