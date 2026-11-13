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

func readStoredPayload(ctx context.Context, key string) ([]byte, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok || content.readOrganization == "" {
		return nil, errContentStorageUnconfigured
	}
	organization := content.readOrganization
	parts := strings.Split(key, "/")
	if len(parts) != 6 || parts[0] != "orgs" || parts[1] != organization {
		return nil, artifact.ErrIntegrity
	}
	expected, err := attachment.ArchiveKey(organization, parts[5])
	if err != nil || expected != key {
		return nil, artifact.ErrIntegrity
	}
	content.mu.Lock()
	raw, found := content.payloads[key]
	ref, known := content.references[key]
	writing := content.writing
	content.mu.Unlock()
	if found {
		if !known || ref.Validate(organization) != nil || ref.Key != key || ref.CustomerOwned || ref.BindingID != "" || ref.SchemaVersion != 1 {
			return nil, artifact.ErrIntegrity
		}
		return bytes.Clone(raw), nil
	}
	if writing {
		return nil, &claimHydration{run: func(ctx context.Context, _ nativeQueryer) error {
			_, err := readStoredPayload(ctx, key)
			return err
		}}
	}
	if content.readReference == nil {
		return nil, errContentStorageUnconfigured
	}
	ref, err = content.readReference(ctx, key)
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

func isolateContentContext(ctx context.Context) context.Context {
	if current, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		current.mu.Lock()
		isolated := &changeContent{encoder: current.encoder, call: current.call, store: current.store, register: current.register, readOrganization: current.readOrganization, readReference: current.readReference, query: current.query, prepared: maps.Clone(current.prepared), payloads: maps.Clone(current.payloads), references: maps.Clone(current.references), payloadBytes: current.payloadBytes, organization: current.organization, stableHistory: current.stableHistory}
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
