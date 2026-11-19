package changefiles

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func PrepareChangeContent(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	return PrepareMeteredContent(ctx, organization, data, true)
}

func contentPreparationKey(organization string, data []byte, billable bool) string {
	key := organization + "/" + artifact.Digest(data)
	if !billable {
		key = "receipt/" + key
	}
	return key
}

func PrepareMeteredContent(ctx context.Context, organization string, data []byte, billable bool) (attachment.ArchiveReference, error) {
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

func (s *Content) RegisterChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference, billable bool) error {
	return s.writeChangeContent(ctx, organization, ref, billable, true)
}

func (s *Content) RegisterConfigurationContent(ctx context.Context, organization string, ref attachment.ArchiveReference) error {
	return s.writeChangeContent(ctx, organization, ref, true, false)
}

func (s *Content) writeChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference, billable, withinAllowance bool) error {
	if err := ref.Validate(organization); err != nil {
		return err
	}
	if s.deps.Repository == nil {
		return hubapi.AttachmentAccountingUnavailable()
	}
	return s.deps.Repository.WriteTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		return s.RegisterChangeContentRecord(ctx, w, organization, ref, billable, withinAllowance)
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

func (s *Content) RegisterChangeContentRecord(ctx context.Context, w tenantstore.Writer, organization string, ref attachment.ArchiveReference, billable, withinAllowance bool) error {
	if err := ref.Validate(organization); err != nil {
		return err
	}
	completion := changeContentCompletion(ctx)
	now := s.deps.Clock().UTC()
	limit := int64(math.MaxInt64)
	plans := s.deps.Plans()
	var entitlement hostedplans.HostedEntitlement
	if plans != nil {
		var err error
		entitlement, err = hubapi.FeatureResult(s.deps.EntitlementRecord(ctx, w, now))
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
	if plans != nil && !completion && ctx.Value(requestmetrics.RunnerBudgetContext{}) != true {
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

func (s *Content) CheckChangeContentAllowance(ctx context.Context, organization string, refs map[string]attachment.ArchiveReference, next attachment.ArchiveReference) error {
	if s.deps.Repository == nil {
		return hubapi.AttachmentAccountingUnavailable()
	}
	plans := s.deps.Plans()
	if plans == nil || changeContentCompletion(ctx) {
		return nil
	}
	now := s.deps.Clock().UTC()
	return s.deps.Repository.ReadTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
		entitlement, err := hubapi.FeatureResult(s.deps.EntitlementRecord(ctx, r, now))
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
		if ctx.Value(requestmetrics.RunnerBudgetContext{}) != true {
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
