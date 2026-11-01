package hubserver

import (
	"context"
	"database/sql"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func useStableRunHistoryIDs(ctx context.Context) {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		content.stableHistory = true
		content.mu.Unlock()
	}
}

func insertContentRegistration(ctx context.Context, tx *sql.Tx, organization string, ref attachment.ArchiveReference, billable int) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO change_content_objects(organization_id,object_sha256,object_key,object_bytes,content_bytes,billable) SELECT ?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='') ON CONFLICT DO UPDATE SET billable=max(change_content_objects.billable,excluded.billable)`, organization, ref.SHA256, ref.Key, ref.CompressedSize, ref.UncompressedSize, billable, organization, ref.Key)
	return err
}

func storedRunPayload(ctx context.Context, organization string, raw []byte) (string, string, error) {
	key, err := storedPayloadKey(ctx, organization, raw)
	return key, "{}", err
}
