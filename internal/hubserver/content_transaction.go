package hubserver

import (
	"context"
	"database/sql"
	"maps"
	"slices"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type contentRegistration struct {
	organization string
	reference    attachment.ArchiveReference
	billable     int
}

func deferContentRegistration(ctx context.Context) {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		if content.registrations == nil {
			content.registrations = make(map[string]contentRegistration)
		}
		content.mu.Unlock()
	}
}

func registerContent(ctx context.Context, content *changeContent, organization string, ref attachment.ArchiveReference, billable bool) error {
	content.mu.Lock()
	if content.registrations != nil {
		key := organization + "/" + ref.Key
		value := content.registrations[key]
		value.organization, value.reference = organization, ref
		if billable {
			value.billable = 1
		}
		content.registrations[key] = value
		content.mu.Unlock()
		return nil
	}
	content.mu.Unlock()
	return content.register(ctx, organization, ref, billable)
}

func registerPreparedContent(ctx context.Context, tx *sql.Tx) error {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return nil
	}
	content.mu.Lock()
	registrations := maps.Clone(content.registrations)
	content.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(registrations)) {
		value := registrations[key]
		if err := insertContentRegistration(ctx, tx, value.organization, value.reference, value.billable); err != nil {
			return err
		}
	}
	return nil
}

func insertContentRegistration(ctx context.Context, tx *sql.Tx, organization string, ref attachment.ArchiveReference, billable int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO change_content_objects(organization_id,object_sha256,object_key,object_bytes,content_bytes,billable) SELECT ?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='') ON CONFLICT DO UPDATE SET billable=max(change_content_objects.billable,excluded.billable)`, organization, ref.SHA256, ref.Key, ref.CompressedSize, ref.UncompressedSize, billable, organization, ref.Key)
	return err
}

func storedRunPayload(ctx context.Context, organization string, raw []byte) (string, string, error) {
	key, err := storedPayloadKey(ctx, organization, raw)
	return key, "{}", err
}
