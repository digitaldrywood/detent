package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errContentStorageUnconfigured = fmt.Errorf("immutable content storage is not configured: %w", artifact.ErrStorage)

func storedPayloadKey(ctx context.Context, organization string, raw []byte) (string, error) {
	return storedMeteredPayloadKey(ctx, organization, raw, true)
}

func storedMeteredPayloadKey(ctx context.Context, organization string, raw []byte, billable bool) (string, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return "", errContentStorageUnconfigured
	}
	content.mu.Lock()
	content.organization = organization
	ref, found := content.prepared[contentPreparationKey(organization, raw, billable)]
	content.mu.Unlock()
	if found {
		return ref.Key, nil
	}
	return "", &claimHydration{run: func(ctx context.Context, _ nativeQueryer) error {
		_, err := prepareMeteredContent(ctx, organization, raw, billable)
		return err
	}}
}

func readStoredPayload(ctx context.Context, key string) ([]byte, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return nil, errContentStorageUnconfigured
	}
	content.mu.Lock()
	raw, found := content.payloads[key]
	writing := content.writing
	content.mu.Unlock()
	if found {
		return bytes.Clone(raw), nil
	}
	if claimWriting(ctx) {
		return nil, errClaimHintStale
	}
	prepared := claimPrepared(ctx)
	if writing || prepared != nil && !prepared.hydrating {
		return nil, &claimHydration{run: func(ctx context.Context, _ nativeQueryer) error {
			_, err := readStoredPayload(ctx, key)
			return err
		}}
	}
	parts := strings.Split(key, "/")
	if len(parts) != 6 || parts[0] != "orgs" {
		return nil, artifact.ErrIntegrity
	}
	organization := parts[1]
	query := content.query
	if scoped, ok := ctx.Value(payloadQueryKey{}).(nativeQueryer); ok {
		query = scoped
	}
	ref, err := readPayloadReference(ctx, query, organization, key)
	if err != nil {
		return nil, fmt.Errorf("read payload reference: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	raw, err = readChangeContent(ctx, organization, ref)
	if err != nil {
		return nil, err
	}
	content.mu.Lock()
	err = content.rememberPayload(ref, raw)
	content.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func payloadNativeID(ctx context.Context, prefix string) string {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return newNativeID(prefix)
	}
	content.mu.Lock()
	defer content.mu.Unlock()
	if content.ids == nil {
		content.ids = make(map[string]string)
	}
	if content.idPositions == nil {
		content.idPositions = make(map[string]int)
	}
	slot := prefix + ":" + strconv.Itoa(content.idPositions[prefix])
	content.idPositions[prefix]++
	if id := content.ids[slot]; id != "" {
		return id
	}
	id := newNativeID(prefix)
	content.ids[slot] = id
	return id
}

func contentCommitted(ctx context.Context) {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		clear(content.ids)
		content.mu.Unlock()
	}
}

func contentWriting(ctx context.Context, writing bool) {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		content.writing = writing
		if writing {
			clear(content.idPositions)
		}
		content.mu.Unlock()
	}
}

func payloadTimestamp(ctx context.Context, now time.Time) time.Time {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return now
	}
	content.mu.Lock()
	defer content.mu.Unlock()
	if content.generatedAt.IsZero() {
		content.generatedAt = now
	}
	return content.generatedAt
}

func (content *changeContent) rememberPayload(ref attachment.ArchiveReference, raw []byte) error {
	key := ref.Key
	if content.references == nil {
		content.references = make(map[string]attachment.ArchiveReference)
	}
	if content.payloads == nil {
		content.payloads = make(map[string][]byte)
	}
	if content.mutationDepth == 0 && (len(content.payloads) >= 8 || content.payloadBytes+int64(len(raw)-len(content.payloads[key])) > attachment.MaxArchiveBytes) {
		clear(content.payloads)
		clear(content.references)
		content.payloadBytes = 0
	}
	if len(content.payloads) >= claimEvidenceLimit || content.payloadBytes+int64(len(raw)-len(content.payloads[key])) > attachment.MaxArchiveBytes {
		return attachment.ErrTooLarge
	}
	content.payloadBytes -= int64(len(content.payloads[key]))
	content.payloads[key] = bytes.Clone(raw)
	content.references[key] = ref
	content.payloadBytes += int64(len(raw))
	return nil
}

func contentMutation(ctx context.Context, active bool) {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		defer content.mu.Unlock()
		if active {
			if content.mutationDepth == 0 {
				clear(content.ids)
				clear(content.idPositions)
				content.generatedAt = time.Time{}
				if len(content.prepared) >= claimEvidenceLimit {
					clear(content.prepared)
				}
			}
			content.mutationDepth++
		} else {
			content.mutationDepth--
		}
	}
}

func readPayloadReference(ctx context.Context, q nativeQueryer, organization, key string) (attachment.ArchiveReference, error) {
	ref := contentReference()
	err := q.QueryRowContext(ctx, `SELECT object_key,object_sha256,object_bytes,content_bytes FROM change_content_objects WHERE organization_id=? AND object_key=?
 UNION ALL SELECT object_key,sha256,size,uncompressed_size FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='' LIMIT 1`, organization, key, organization, key).Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize)
	return ref, err
}

func prepareContentMutation[T any](ctx context.Context, q nativeQueryer, operation func() (T, error)) (T, error) {
	contentMutation(ctx, true)
	defer contentMutation(ctx, false)
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		content = nil
	}
	previousWriting := false
	var previousPositions map[string]int
	if content != nil {
		content.mu.Lock()
		previousWriting = content.writing
		previousPositions = maps.Clone(content.idPositions)
		content.mu.Unlock()
	}
	defer func() {
		if content != nil {
			content.mu.Lock()
			content.writing = previousWriting
			if previousWriting {
				content.idPositions = previousPositions
			}
			content.mu.Unlock()
		}
	}()
	for {
		contentWriting(ctx, true)
		result, err := operation()
		contentWriting(ctx, false)
		var hydration *claimHydration
		if !errors.As(err, &hydration) {
			return result, err
		}
		if err := hydration.run(ctx, q); err != nil {
			var zero T
			return zero, err
		}
	}
}

func isolateContentContext(ctx context.Context) context.Context {
	if current, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		current.mu.Lock()
		isolated := &changeContent{call: current.call, store: current.store, register: current.register, query: current.query, prepared: maps.Clone(current.prepared), payloads: maps.Clone(current.payloads), references: maps.Clone(current.references), payloadBytes: current.payloadBytes, organization: current.organization}
		current.mu.Unlock()
		ctx = context.WithValue(ctx, changeContentKey{}, isolated)
	}
	if current, ok := ctx.Value(runDetailContextKey{}).(*runDetailContext); ok {
		current.mu.Lock()
		isolated := &runDetailContext{store: current.store, organization: current.organization, cache: make(map[string]map[string]tracker.NativeRunData), reference: current.reference, runs: maps.Clone(current.runs), cancellation: current.cancellation}
		current.mu.Unlock()
		ctx = context.WithValue(ctx, runDetailContextKey{}, isolated)
	}
	return ctx
}

type payloadQueryKey struct{}

func payloadQueryContext(ctx context.Context, q nativeQueryer) context.Context {
	return context.WithValue(ctx, payloadQueryKey{}, q)
}
