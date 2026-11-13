package hubcontent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeContentKey struct{}

func ImmutableContentStorageError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, artifact.ErrIntegrity) {
		return err
	}
	return fmt.Errorf("immutable content storage: %w", errors.Join(artifact.ErrStorage, err))
}

type ChangeContent struct {
	Encoder          *attachment.ArchiveEncoder                                             `json:"-"`
	Mu               sync.Mutex                                                             `json:"-"`
	Call             entryauthority.ContentHandler                                          `json:"-"`
	Store            attachment.DetailStore                                                 `json:"-"`
	Register         func(context.Context, string, attachment.ArchiveReference, bool) error `json:"-"`
	ReadOrganization string                                                                 `json:"-"`
	ReadReference    func(context.Context, string) (attachment.ArchiveReference, error)     `json:"-"`
	Prepared         map[string]attachment.ArchiveReference                                 `json:"-"`
	Query            hubapi.NativeQueryer                                                   `json:"-"`
	Payloads         map[string][]byte                                                      `json:"-"`
	References       map[string]attachment.ArchiveReference                                 `json:"-"`
	ids              map[string]string
	IDPositions      map[string]int `json:"-"`
	generatedAt      time.Time
	Organization     string `json:"-"`
	stableHistory    bool
	Completion       bool `json:"-"`
	MutationDepth    int  `json:"-"`
	payloadBytes     int64
	Writing          bool `json:"-"`
}

func ReadChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, artifact.ErrIntegrity
	}
	return ReadRuntimeObject(ctx, NativeRuntimeObjectKey{Kind: "content", Organization: organization, Reference: ref}, func() ([]byte, error) {
		return ReadChangeContentObject(ctx, organization, ref)
	})
}

func ReadChangeContentObject(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	content, ok := ctx.Value(ChangeContentKey{}).(*ChangeContent)
	if !ok {
		return nil, ErrContentStorageUnconfigured
	}
	content.Mu.Lock()
	writing := content.Writing
	cached, found := content.Payloads[ref.Key]
	cachedRef := content.References[ref.Key]
	content.Mu.Unlock()
	if writing {
		if found && cachedRef == ref {
			return bytes.Clone(cached), nil
		}
		return nil, &ClaimHydration{Run: func(ctx context.Context, _ hubapi.NativeQueryer) error {
			raw, err := ReadChangeContent(ctx, organization, ref)
			if err != nil {
				return err
			}
			content.Mu.Lock()
			defer content.Mu.Unlock()
			return content.RememberPayload(ref, raw)
		}}
	}
	return ReadPublishedContent(ctx, content, organization, ref)
}

func ReadPublishedContent(ctx context.Context, content *ChangeContent, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if content.Call == nil {
		raw, err := content.Store.ReadDetail(ctx, organization, ref)
		if err != nil {
			return nil, ImmutableContentStorageError(err)
		}
		if int64(len(raw)) != ref.UncompressedSize {
			return nil, artifact.ErrIntegrity
		}
		return raw, nil
	}
	compressed, err := content.Call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
	if err != nil {
		return nil, ImmutableContentStorageError(err)
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

func ContentReference() attachment.ArchiveReference {
	return attachment.ArchiveReference{Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
}

var ErrContentStorageUnconfigured = fmt.Errorf("immutable content storage is not configured: %w", artifact.ErrStorage)

func ReadStoredPayload(ctx context.Context, key string) ([]byte, error) {
	content, ok := ctx.Value(ChangeContentKey{}).(*ChangeContent)
	if !ok || content.ReadOrganization == "" {
		return nil, ErrContentStorageUnconfigured
	}
	organization := content.ReadOrganization
	parts := strings.Split(key, "/")
	if len(parts) != 6 || parts[0] != "orgs" || parts[1] != organization {
		return nil, artifact.ErrIntegrity
	}
	expected, err := attachment.ArchiveKey(organization, parts[5])
	if err != nil || expected != key {
		return nil, artifact.ErrIntegrity
	}
	content.Mu.Lock()
	raw, found := content.Payloads[key]
	ref, known := content.References[key]
	writing := content.Writing
	content.Mu.Unlock()
	if found {
		if !known || ref.Validate(organization) != nil || ref.Key != key || ref.CustomerOwned || ref.BindingID != "" || ref.SchemaVersion != 1 {
			return nil, artifact.ErrIntegrity
		}
		return bytes.Clone(raw), nil
	}
	if writing {
		return nil, &ClaimHydration{Run: func(ctx context.Context, _ hubapi.NativeQueryer) error {
			_, err := ReadStoredPayload(ctx, key)
			return err
		}}
	}
	if content.ReadReference == nil {
		return nil, ErrContentStorageUnconfigured
	}
	ref, err = content.ReadReference(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read payload reference: %w", errors.Join(artifact.ErrIntegrity, err))
	}
	raw, err = ReadChangeContent(ctx, organization, ref)
	if err != nil {
		return nil, err
	}
	content.Mu.Lock()
	err = content.RememberPayload(ref, raw)
	content.Mu.Unlock()
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func PayloadNativeID(ctx context.Context, prefix string) string {
	content, ok := ctx.Value(ChangeContentKey{}).(*ChangeContent)
	if !ok {
		return hubapi.NewID(prefix)
	}
	content.Mu.Lock()
	defer content.Mu.Unlock()
	if content.ids == nil {
		content.ids = make(map[string]string)
	}
	if content.IDPositions == nil {
		content.IDPositions = make(map[string]int)
	}
	slot := prefix + ":" + strconv.Itoa(content.IDPositions[prefix])
	content.IDPositions[prefix]++
	if id := content.ids[slot]; id != "" {
		return id
	}
	id := hubapi.NewID(prefix)
	content.ids[slot] = id
	return id
}

func PayloadTimestamp(ctx context.Context, now time.Time) time.Time {
	content, ok := ctx.Value(ChangeContentKey{}).(*ChangeContent)
	if !ok {
		return now
	}
	content.Mu.Lock()
	defer content.Mu.Unlock()
	if content.generatedAt.IsZero() {
		content.generatedAt = now
	}
	return content.generatedAt
}

func (content *ChangeContent) RememberPayload(ref attachment.ArchiveReference, raw []byte) error {
	key := ref.Key
	if content.References == nil {
		content.References = make(map[string]attachment.ArchiveReference)
	}
	if content.Payloads == nil {
		content.Payloads = make(map[string][]byte)
	}
	if content.MutationDepth == 0 && (len(content.Payloads) >= 8 || content.payloadBytes+int64(len(raw)-len(content.Payloads[key])) > attachment.MaxArchiveBytes) {
		clear(content.Payloads)
		clear(content.References)
		content.payloadBytes = 0
	}
	if len(content.Payloads) >= ClaimEvidenceLimit || content.payloadBytes+int64(len(raw)-len(content.Payloads[key])) > attachment.MaxArchiveBytes {
		return attachment.ErrTooLarge
	}
	content.payloadBytes -= int64(len(content.Payloads[key]))
	content.Payloads[key] = bytes.Clone(raw)
	content.References[key] = ref
	content.payloadBytes += int64(len(raw))
	return nil
}

func IsolateContentContext(ctx context.Context) context.Context {
	if current, ok := ctx.Value(ChangeContentKey{}).(*ChangeContent); ok {
		current.Mu.Lock()
		isolated := &ChangeContent{Encoder: current.Encoder, Call: current.Call, Store: current.Store, Register: current.Register, ReadOrganization: current.ReadOrganization, ReadReference: current.ReadReference, Query: current.Query, Prepared: maps.Clone(current.Prepared), Payloads: maps.Clone(current.Payloads), References: maps.Clone(current.References), payloadBytes: current.payloadBytes, Organization: current.Organization, stableHistory: current.stableHistory}
		current.Mu.Unlock()
		ctx = context.WithValue(ctx, ChangeContentKey{}, isolated)
	}
	if current, ok := ctx.Value(RunDetailContextKey{}).(*RunDetailContext); ok {
		current.Mu.Lock()
		isolated := &RunDetailContext{Store: current.Store, Organization: current.Organization, Cache: make(map[string]map[string]tracker.NativeRunData), Reference: current.Reference, Runs: maps.Clone(current.Runs), cancellation: current.cancellation}
		current.Mu.Unlock()
		ctx = context.WithValue(ctx, RunDetailContextKey{}, isolated)
	}
	return ctx
}
