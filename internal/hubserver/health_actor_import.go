package hubserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

func normalizeImportedHealthActors(ctx context.Context, store attachment.DetailStore, record *postgres.CoreWorkItemImport) (bool, error) {
	normalize := func(kind *string, principal *string) bool {
		if kind == nil || principal == nil || *kind != "system" || *principal != "health_detector" {
			return false
		}
		*kind = "integration"
		return true
	}
	changed := normalize(record.Item.ActorKind, record.Item.ActorPrincipalID)
	for i := range record.Comments {
		comment := &record.Comments[i]
		changed = normalize(&comment.ActorKind, &comment.ActorPrincipalID) || changed
		changed = normalize(comment.EditedByKind, comment.EditedByPrincipalID) || changed
	}
	for i := range record.Versions {
		row := &record.Versions[i]
		raw, err := store.ReadDetail(ctx, string(row.OrganizationID), eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes))
		if err != nil {
			return false, fmt.Errorf("read imported health version: %w", err)
		}
		raw, normalized, err := normalizeImportedHealthActorJSON(raw)
		if err != nil {
			return false, err
		}
		if !normalized {
			continue
		}
		ref, err := store.StoreDetail(ctx, string(row.OrganizationID), raw)
		if err != nil {
			return false, fmt.Errorf("store imported health version: %w", err)
		}
		row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
		changed = true
	}
	for i := range record.Events {
		row := &record.Events[i]
		if row.ActorKind != "system" || row.ActorPrincipalID != "health_detector" {
			continue
		}
		raw, err := store.ReadDetail(ctx, string(row.OrganizationID), eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes))
		if err != nil {
			return false, fmt.Errorf("read imported health event: %w", err)
		}
		raw, normalized, err := normalizeImportedHealthActorJSON(raw)
		if err != nil {
			return false, err
		}
		if !normalized {
			return false, eventstore.ErrInvalidRecord
		}
		ref, err := store.StoreDetail(ctx, string(row.OrganizationID), raw)
		if err != nil {
			return false, fmt.Errorf("store imported health event: %w", err)
		}
		row.ActorKind = "integration"
		row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
		changed = true
	}
	return changed, nil
}

func normalizeImportedHealthActorJSON(raw []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false, eventstore.ErrInvalidRecord
	}
	changed := false
	for _, key := range []string{"actor", "edited_by"} {
		value, exists := fields[key]
		if !exists || string(value) == "null" {
			continue
		}
		var actor map[string]json.RawMessage
		if err := json.Unmarshal(value, &actor); err != nil {
			return nil, false, eventstore.ErrInvalidRecord
		}
		var kind, principal string
		if json.Unmarshal(actor["kind"], &kind) != nil || json.Unmarshal(actor["principal_id"], &principal) != nil || kind != "system" || principal != "health_detector" {
			continue
		}
		actor["kind"] = json.RawMessage(`"integration"`)
		encoded, err := json.Marshal(actor)
		if err != nil {
			return nil, false, fmt.Errorf("encode imported health actor: %w", err)
		}
		fields[key] = encoded
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, false, fmt.Errorf("encode imported health record: %w", err)
	}
	return encoded, true, nil
}
