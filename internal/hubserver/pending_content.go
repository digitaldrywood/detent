package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
)

const pendingContentSelection = `SELECT pending_content FROM change_content_objects WHERE organization_id=? AND object_key=? AND pending_content IS NOT NULL
UNION ALL SELECT pending_content FROM attachments WHERE organization_id=? AND object_key=? AND pending_content IS NOT NULL LIMIT 1`

type pendingContentKey struct{}

type pendingContentObject struct {
	organization string
	reference    attachment.ArchiveReference
}

type pendingContentMutation struct {
	tx      *sql.Tx
	objects map[string]pendingContentObject
}

func (p *pendingContentMutation) stage(ctx context.Context, organization string, raw []byte, billable bool) (string, error) {
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return "", errContentStorageUnconfigured
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	if _, exists := p.objects[ref.Key]; exists {
		return ref.Key, nil
	}
	_, err = p.tx.ExecContext(ctx, `INSERT INTO change_content_objects(organization_id,object_sha256,object_key,object_bytes,content_bytes,billable,pending_content)
SELECT ?,?,?,?,?,?,? WHERE NOT EXISTS(SELECT 1 FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='')
ON CONFLICT DO UPDATE SET billable=max(change_content_objects.billable,excluded.billable)`, organization, ref.SHA256, ref.Key, ref.CompressedSize, ref.UncompressedSize, billable, raw, organization, ref.Key)
	if err != nil {
		return "", err
	}
	content.mu.Lock()
	err = content.rememberPayload(ref, raw)
	if err == nil {
		if content.prepared == nil {
			content.prepared = make(map[string]attachment.ArchiveReference)
		}
		content.prepared[contentPreparationKey(organization, raw, billable)] = ref
	}
	content.mu.Unlock()
	if err != nil {
		return "", err
	}
	if p.objects == nil {
		p.objects = make(map[string]pendingContentObject)
	}
	p.objects[ref.Key] = pendingContentObject{organization: organization, reference: ref}
	return ref.Key, nil
}

func (s *Service) uploadPendingContent(ctx context.Context, objects map[string]pendingContentObject) ([]pendingContentObject, error) {
	if len(objects) == 0 {
		return nil, nil
	}
	content, ok := ctx.Value(changeContentKey{}).(*changeContent)
	if !ok {
		return nil, errContentStorageUnconfigured
	}
	var published []pendingContentObject
	var failures error
	for _, object := range objects {
		ref := object.reference
		var raw []byte
		err := s.database.reader.QueryRowContext(ctx, pendingContentSelection, object.organization, ref.Key, object.organization, ref.Key).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err == nil {
			err = publishPendingObject(ctx, content, object, raw)
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		published = append(published, object)
	}
	return published, failures
}

func (s *Service) publishPendingContent(ctx context.Context, objects map[string]pendingContentObject) error {
	published, failures := s.uploadPendingContent(ctx, objects)
	if len(published) == 0 {
		return failures
	}
	err := s.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
		for _, object := range published {
			ref := object.reference
			if _, err := tx.ExecContext(ctx, `UPDATE change_content_objects SET pending_content=NULL WHERE organization_id=? AND object_key=? AND object_sha256=? AND object_bytes=? AND content_bytes=? AND pending_content IS NOT NULL`, object.organization, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attachments SET pending_content=NULL WHERE organization_id=? AND object_key=? AND sha256=? AND size=? AND uncompressed_size=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='' AND pending_content IS NOT NULL`, object.organization, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize); err != nil {
				return err
			}
		}
		return nil
	})
	return errors.Join(failures, err)
}

func publishPendingObject(ctx context.Context, content *changeContent, object pendingContentObject, raw []byte) error {
	ref, compressed, err := attachment.PrepareArchive(object.organization, 1, bytes.NewReader(raw))
	if err != nil || ref != object.reference {
		return artifact.ErrIntegrity
	}
	stored, readErr := readPublishedContent(ctx, content, object.organization, ref)
	if readErr == nil {
		if !bytes.Equal(stored, raw) {
			return artifact.ErrIntegrity
		}
		return nil
	}
	if !errors.Is(readErr, artifact.ErrMissing) {
		return readErr
	}
	if content.call == nil {
		var actual attachment.ArchiveReference
		actual, err = content.store.StoreDetail(ctx, object.organization, raw)
		if err == nil && actual != ref {
			return artifact.ErrIntegrity
		}
	} else {
		_, err = content.call(ctx, entryauthority.ContentRequest{Organization: object.organization, Reference: ref, Data: compressed, Write: true})
	}
	stored, readErr = readPublishedContent(ctx, content, object.organization, ref)
	if readErr != nil {
		return fmt.Errorf("reconcile pending content upload: %w", errors.Join(err, readErr))
	}
	if !bytes.Equal(stored, raw) {
		return artifact.ErrIntegrity
	}
	return nil
}

func (s *Service) convertPendingContent(ctx context.Context) error {
	candidates, err := s.storageProjectionPage(ctx, "pending-content", `SELECT object_key FROM (
SELECT organization_id,object_key FROM change_content_objects WHERE pending_content IS NOT NULL
UNION ALL SELECT organization_id,object_key FROM attachments WHERE pending_content IS NOT NULL) WHERE 1=1`, "organization_id,object_key", storageConversionLimit(ctx))
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		organization, ok := candidate.cursor[0].(string)
		if !ok {
			return artifact.ErrIntegrity
		}
		ref, err := readPayloadReference(ctx, s.database.reader, organization, candidate.id)
		if err != nil {
			return err
		}
		if err := chargeStorageConversion(ctx, int(ref.UncompressedSize)); err != nil {
			return err
		}
		s.projectionCursors.Store("pending-content", candidate.cursor)
		if err := s.publishPendingContent(ctx, map[string]pendingContentObject{ref.Key: {organization: organization, reference: ref}}); err != nil {
			return err
		}
	}
	return nil
}
