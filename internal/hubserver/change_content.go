package hubserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeContentKey struct{}

func immutableContentStorageError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, artifact.ErrIntegrity) {
		return err
	}
	return fmt.Errorf("immutable content storage: %w", errors.Join(artifact.ErrStorage, err))
}

type changeContent struct {
	encoder          *attachment.ArchiveEncoder
	mu               sync.Mutex
	call             entryauthority.ContentHandler
	store            attachment.DetailStore
	register         func(context.Context, string, attachment.ArchiveReference, bool) error
	readOrganization string
	readReference    func(context.Context, string) (attachment.ArchiveReference, error)
	prepared         map[string]attachment.ArchiveReference
	query            hubapi.NativeQueryer
	payloads         map[string][]byte
	references       map[string]attachment.ArchiveReference
	ids              map[string]string
	idPositions      map[string]int
	generatedAt      time.Time
	organization     string
	stableHistory    bool
	completion       bool
	mutationDepth    int
	payloadBytes     int64
	writing          bool
}

func (s *Service) contentContext(ctx context.Context) context.Context {
	if ctx.Value(changeContentKey{}) != nil {
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
			return attachment.ArchiveReference{}, errContentStorageUnconfigured
		}
		var ref attachment.ArchiveReference
		err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(attemptArchiveReader)
			if !ok {
				return errContentStorageUnconfigured
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
	return context.WithValue(ctx, changeContentKey{}, &changeContent{encoder: &s.archiveEncoder, call: call, store: store, register: s.registerChangeContent, readOrganization: string(organization), readReference: readReference})
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
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return attachment.ArchiveReference{}, errContentStorageUnconfigured
	}
	content.mu.Lock()
	digest := contentPreparationKey(organization, data, billable)
	cached, found := content.prepared[digest]
	content.mu.Unlock()
	if found {
		return cached, nil
	}
	var ref attachment.ArchiveReference
	if content.call == nil {
		var err error
		ref, err = content.store.StoreDetail(ctx, organization, data)
		if err != nil {
			return ref, immutableContentStorageError(err)
		}
		if ref.Validate(organization) != nil || ref.UncompressedSize != int64(len(data)) {
			return ref, artifact.ErrIntegrity
		}
		verified, err := content.store.ReadDetail(ctx, organization, ref)
		if err != nil {
			return ref, immutableContentStorageError(err)
		}
		if !bytes.Equal(data, verified) {
			return ref, artifact.ErrIntegrity
		}
		if err := content.register(ctx, organization, ref, billable); err != nil {
			return ref, err
		}
	} else {
		encode := attachment.PrepareArchive
		if content.encoder != nil {
			encode = content.encoder.Prepare
		}
		prepared, compressed, err := encode(organization, 1, bytes.NewReader(data))
		if err != nil {
			return prepared, err
		}
		ref = prepared
		if err := content.register(ctx, organization, ref, billable); err != nil {
			return ref, err
		}
		if _, err := content.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Data: compressed, Write: true}); err != nil {
			return ref, immutableContentStorageError(err)
		}
		verified, err := content.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
		if err != nil {
			return ref, immutableContentStorageError(err)
		}
		if !bytes.Equal(compressed, verified) {
			return ref, artifact.ErrIntegrity
		}
	}
	content.mu.Lock()
	defer content.mu.Unlock()
	if content.prepared == nil {
		content.prepared = make(map[string]attachment.ArchiveReference)
	}
	if len(content.prepared) >= claimEvidenceLimit && content.mutationDepth == 0 {
		clear(content.prepared)
	}
	if len(content.prepared) >= claimEvidenceLimit {
		return ref, attachment.ErrTooLarge
	}
	content.prepared[digest] = ref
	if err := content.rememberPayload(ref, data); err != nil {
		return ref, err
	}
	return ref, nil
}

func readChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, artifact.ErrIntegrity
	}
	return readRuntimeObject(ctx, nativeRuntimeObjectKey{kind: "content", organization: organization, reference: ref}, func() ([]byte, error) {
		return readChangeContentObject(ctx, organization, ref)
	})
}

func readChangeContentObject(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return nil, errContentStorageUnconfigured
	}
	content.mu.Lock()
	writing := content.writing
	cached, found := content.payloads[ref.Key]
	cachedRef := content.references[ref.Key]
	content.mu.Unlock()
	if writing {
		if found && cachedRef == ref {
			return bytes.Clone(cached), nil
		}
		return nil, &claimHydration{run: func(ctx context.Context, _ hubapi.NativeQueryer) error {
			raw, err := readChangeContent(ctx, organization, ref)
			if err != nil {
				return err
			}
			content.mu.Lock()
			defer content.mu.Unlock()
			return content.rememberPayload(ref, raw)
		}}
	}
	return readPublishedContent(ctx, content, organization, ref)
}

func readPublishedContent(ctx context.Context, content *changeContent, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if content.call == nil {
		raw, err := content.store.ReadDetail(ctx, organization, ref)
		if err != nil {
			return nil, immutableContentStorageError(err)
		}
		if int64(len(raw)) != ref.UncompressedSize {
			return nil, artifact.ErrIntegrity
		}
		return raw, nil
	}
	compressed, err := content.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
	if err != nil {
		return nil, immutableContentStorageError(err)
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	input := bytes.NewReader(compressed)
	reader, err := gzip.NewReader(input)
	if err != nil {
		return nil, artifact.ErrIntegrity
	}
	reader.Multistream(false)
	data, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || input.Len() != 0 || int64(len(data)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}

func contentReference() attachment.ArchiveReference {
	return attachment.ArchiveReference{Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
}

func (s *Service) registerChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference, billable bool) error {
	if err := ref.Validate(organization); err != nil {
		return err
	}
	if s.config.TenantRepository == nil {
		return hubapi.AttachmentAccountingUnavailable()
	}
	return s.config.TenantRepository.WriteTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		return s.registerChangeContentRecord(ctx, w, organization, ref, billable)
	})
}

func changeContentCompletion(ctx context.Context) bool {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		defer content.mu.Unlock()
		return content.completion
	}
	return false
}

func (s *Service) registerChangeContentRecord(ctx context.Context, w tenantstore.Writer, organization string, ref attachment.ArchiveReference, billable bool) error {
	if err := ref.Validate(organization); err != nil {
		return err
	}
	completion := changeContentCompletion(ctx)
	now := s.config.now().UTC()
	limit := int64(math.MaxInt64)
	plans := s.database.plans()
	var entitlement HostedEntitlement
	if plans != nil {
		var err error
		entitlement, err = s.database.hostedEntitlementRecord(ctx, w, now)
		if err != nil {
			return err
		}
		if allowance, ok := entitlement.Allowances["collaboration_bytes"]; ok && !completion {
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
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
	}
	if plans != nil && !completion && ctx.Value(runnerRequestBudgetContext{}) != true {
		window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
		used, err := w.BillingUsage(ctx, window, "api_mutations")
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if used >= entitlement.Allowances["api_mutations"] {
			return &hostedLimitError{Resource: "api_mutations", Allowance: entitlement.Allowances["api_mutations"], Consumption: used}
		}
	}
	return nil
}

func (s *Service) checkChangeContentAllowance(ctx context.Context, organization string, refs map[string]attachment.ArchiveReference, next attachment.ArchiveReference) error {
	if s.config.TenantRepository == nil {
		return hubapi.AttachmentAccountingUnavailable()
	}
	plans := s.database.plans()
	if plans == nil || changeContentCompletion(ctx) {
		return nil
	}
	now := s.config.now().UTC()
	return s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
		entitlement, err := s.database.hostedEntitlementRecord(ctx, r, now)
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
					return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
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
				return &hostedLimitError{Resource: "api_mutations", Allowance: entitlement.Allowances["api_mutations"], Consumption: used}
			}
		}
		return nil
	})
}
