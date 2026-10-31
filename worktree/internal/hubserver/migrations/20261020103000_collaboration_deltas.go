package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type EncodeVersion func(previous []byte, previousEncoding string, raw []byte, revision int64) ([]byte, string, error)
type ReadVersion func(context.Context, *sql.Tx, string, string, string, string, int64) ([]byte, error)

func CollaborationDeltas(ctx context.Context, tx *sql.Tx, encode EncodeVersion, read ReadVersion, recount func(context.Context, *sql.Tx) error) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE collaboration_versions ADD COLUMN encoding TEXT NOT NULL DEFAULT 'snapshot' CHECK (encoding IN ('snapshot','patch'));
DROP TRIGGER collaboration_versions_no_update;
DROP TRIGGER collaboration_versions_no_delete;`); err != nil {
		return err
	}
	var organization, record string
	var revision int64
	var previous []byte
	var previousEncoding string
	for {
		var nextOrganization, project, item, nextRecord string
		var nextRevision int64
		var original []byte
		err := tx.QueryRowContext(ctx, `SELECT organization_id, project_id, work_item_id, record_id, revision, record_json FROM collaboration_versions
WHERE (organization_id, record_id, revision) > (?, ?, ?) ORDER BY organization_id, record_id, revision LIMIT 1`, organization, record, revision).Scan(&nextOrganization, &project, &item, &nextRecord, &nextRevision, &original)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		if nextOrganization != organization || nextRecord != record || nextRevision != revision+1 {
			previous = nil
			previousEncoding = ""
		}
		encoded, encoding, err := encode(previous, previousEncoding, original, nextRevision)
		if err != nil {
			return fmt.Errorf("encode collaboration history: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collaboration_versions SET record_json=?,encoding=? WHERE organization_id=? AND record_id=? AND revision=?`, string(encoded), encoding, nextOrganization, nextRecord, nextRevision); err != nil {
			return err
		}
		reconstructed, err := read(ctx, tx, nextOrganization, project, item, nextRecord, nextRevision)
		if err != nil {
			return fmt.Errorf("verify collaboration history: %w", err)
		}
		if !bytes.Equal(reconstructed, original) {
			return errors.New("collaboration history conversion changed a revision")
		}
		organization, record, revision = nextOrganization, nextRecord, nextRevision
		previous, previousEncoding = encoded, encoding
	}
	if _, err := tx.ExecContext(ctx, `CREATE TRIGGER collaboration_versions_no_update BEFORE UPDATE ON collaboration_versions
BEGIN
 SELECT RAISE(ABORT, 'collaboration history is append-only');
END;
CREATE TRIGGER collaboration_versions_no_delete BEFORE DELETE ON collaboration_versions
BEGIN
 SELECT RAISE(ABORT, 'collaboration history is append-only');
END;`); err != nil {
		return err
	}
	return recount(ctx, tx)
}
