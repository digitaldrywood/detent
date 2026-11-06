package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreChangeImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	SourceObjects  attachment.DetailStore
	Objects        attachment.DetailStore
}

func ImportCoreChanges(ctx context.Context, target *postgres.Repository, source CoreChangeImportSource) (int64, error) {
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return 0, errors.New("invalid core change import source")
	}
	content := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := target.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
			return err
		}
		after := ""
		for {
			changes, err := readCoreChangePage(ctx, tx, source, after)
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				break
			}
			for _, row := range changes {
				if err := importCoreChange(ctx, tx, target, content, source, copyHash, row); err != nil {
					return fmt.Errorf("import core change %s: %w", row.change.ID, err)
				}
				imported++
				if err := importCoreChangeVersions(ctx, tx, target, content, source, copyHash, row.change.ID); err != nil {
					return fmt.Errorf("import core change versions: %w", err)
				}
				if err := importCoreChangePublications(ctx, tx, target, content, source, copyHash, row.change.ID); err != nil {
					return fmt.Errorf("import core change publications: %w", err)
				}
				if err := importCoreChangeEvidence(ctx, tx, target, content, source, copyHash, row.change.ID); err != nil {
					return fmt.Errorf("import core change evidence: %w", err)
				}
				if err := importCoreChangeLandings(ctx, tx, target, content, source, copyHash, row.change.ID); err != nil {
					return fmt.Errorf("import core change landing receipts: %w", err)
				}
				if err := importCoreChangeViewedFiles(ctx, tx, target, content, source, copyHash, row.change.ID); err != nil {
					return fmt.Errorf("import core change viewed files: %w", err)
				}
				if err := finalizeCoreChange(ctx, target, content, source, copyHash, row.change); err != nil {
					return fmt.Errorf("finalize core change %s: %w", row.change.ID, err)
				}
			}
			after = changes[len(changes)-1].change.ID
		}
		return importCoreReleaseDeliveries(ctx, tx, target, content, source, copyHash)
	})
	if err != nil {
		return imported, err
	}
	return imported, nil
}

type coreChangeImportRow struct {
	change tracker.ChangeRequest
	links  []tracker.NativeWorkItemID
}

func readCoreChangePage(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, after string) ([]coreChangeImportRow, error) {
	organization := source.OrganizationID
	rows, err := tx.QueryContext(ctx, `SELECT id,project_id,work_item_id,record_json FROM change_requests WHERE organization_id=? AND id>? ORDER BY id LIMIT 100`, organization, after)
	if err != nil {
		return nil, err
	}
	result := make([]coreChangeImportRow, 0, 100)
	for rows.Next() {
		var id, project, item string
		var raw []byte
		if err := rows.Scan(&id, &project, &item, &raw); err != nil {
			return nil, err
		}
		var change tracker.ChangeRequest
		if err := json.Unmarshal(raw, &change); err != nil || change.ID != id || change.OrganizationID != organization || change.ProjectID != tracker.ProjectID(project) || change.WorkItemID != tracker.NativeWorkItemID(item) || change.Revision < 1 || change.CreatedAt.IsZero() || change.UpdatedAt.IsZero() {
			return nil, errors.Join(eventstore.ErrInvalidRecord, err)
		}
		links, err := readCoreChangeLinks(ctx, tx, organization, id, project)
		if err != nil {
			return nil, err
		}
		change.LinkedIssues = links
		if err := restoreCoreReportLanding(ctx, tx, source, &change); err != nil {
			return nil, err
		}
		result = append(result, coreChangeImportRow{change: change, links: links})
	}
	return result, closeCoreChangeRows(rows)
}

func closeCoreChangeRows(rows *sql.Rows) error {
	defer rows.Close()
	return rows.Err()
}

func readCoreChangeLinks(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, changeID, project string) ([]tracker.NativeWorkItemID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,work_item_id FROM change_issue_links WHERE organization_id=? AND change_id=? ORDER BY work_item_id LIMIT 33`, organization, changeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]tracker.NativeWorkItemID, 0, 32)
	for rows.Next() {
		var linkProject, item string
		if err := rows.Scan(&linkProject, &item); err != nil {
			return nil, err
		}
		if linkProject != project || item == "" || len(links) == 32 {
			return nil, eventstore.ErrInvalidRecord
		}
		links = append(links, tracker.NativeWorkItemID(item))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, eventstore.ErrInvalidRecord
	}
	return links, nil
}

func importCoreChange(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash string, row coreChangeImportRow) error {
	raw, err := json.Marshal(row.change)
	if err != nil {
		return err
	}
	record, err := content.PrepareChange(ctx, source.OrganizationID, raw)
	if err != nil {
		return err
	}
	initial := row.change
	initial.CurrentVersion = ""
	initialRaw, err := json.Marshal(initial)
	if err != nil {
		return err
	}
	initialRecord, err := content.PrepareChange(ctx, source.OrganizationID, initialRaw)
	if err != nil {
		return err
	}
	receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change", row.change.ID, struct {
		Change eventstore.Change          `json:"change"`
		Links  []tracker.NativeWorkItemID `json:"links"`
	}{record, row.links})
	if err != nil {
		return err
	}
	return target.WriteRecords(ctx, source.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
		replayed, err := checkCoreChangeReceipt(ctx, w, receipt)
		if err != nil || replayed {
			return err
		}
		if _, err := w.CreateChange(ctx, initialRecord); err != nil {
			return err
		}
		for _, item := range row.links {
			if err := w.LinkIssue(ctx, eventstore.Link{OrganizationID: source.OrganizationID, ProjectID: row.change.ProjectID, WorkItemID: item, ChangeID: row.change.ID}); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
}

func finalizeCoreChange(ctx context.Context, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash string, change tracker.ChangeRequest) error {
	raw, err := json.Marshal(change)
	if err != nil {
		return err
	}
	record, err := content.PrepareChange(ctx, source.OrganizationID, raw)
	if err != nil {
		return err
	}
	receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change", change.ID, struct {
		Change eventstore.Change          `json:"change"`
		Links  []tracker.NativeWorkItemID `json:"links"`
	}{record, change.LinkedIssues})
	if err != nil {
		return err
	}
	return target.FinalizeCoreChangeImport(ctx, record, receipt)
}

func checkCoreChangeReceipt(ctx context.Context, w eventstore.Writer, receipt eventstore.Receipt) (bool, error) {
	previous, err := w.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
	if err == nil {
		if previous.RequestHash != receipt.RequestHash {
			return false, corestore.ErrRevisionConflict
		}
		return true, nil
	}
	if !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	return false, nil
}

func coreChangeSourceContext(ctx context.Context, tx *sql.Tx, objects attachment.DetailStore) context.Context {
	ctx = context.WithValue(ctx, changeContentKey{}, &changeContent{store: objects, query: tx})
	return payloadQueryContext(ctx, tx)
}

func readCoreChangeObject(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, key string) ([]byte, error) {
	var ref attachment.ArchiveReference
	ref.Codec = "gzip"
	ref.SchemaVersion = 1
	ref.Kind = "archive"
	ref.Retention = attachment.ArchiveRetention
	err := tx.QueryRowContext(ctx, `SELECT object_key,object_sha256,object_bytes,content_bytes FROM change_content_objects WHERE organization_id=? AND object_key=?
UNION ALL SELECT object_key,sha256,size,uncompressed_size FROM attachments WHERE organization_id=? AND object_key=? AND kind='archive' AND customer_owned=0 AND storage_binding_id='' LIMIT 1`, source.OrganizationID, key, source.OrganizationID, key).Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize)
	if err != nil {
		return nil, err
	}
	if err := ref.Validate(string(source.OrganizationID)); err != nil {
		return nil, err
	}
	data, err := source.SourceObjects.ReadDetail(ctx, string(source.OrganizationID), ref)
	if err != nil || int64(len(data)) != ref.UncompressedSize {
		return nil, errors.Join(err, eventstore.ErrInvalidRecord)
	}
	return data, nil
}

func readCoreChangePayload(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, raw string) ([]byte, error) {
	if raw == "" {
		return nil, eventstore.ErrInvalidRecord
	}
	if json.Valid([]byte(raw)) {
		return []byte(raw), nil
	}
	return readCoreChangeObject(ctx, tx, source, raw)
}

type coreChangeEvidenceRow struct {
	sequence                   int64
	kind, versionID, sourceKey string
	raw                        []byte
}

func readCoreChangeEvidencePage(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, changeID string, after int64) ([]coreChangeEvidenceRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,kind,COALESCE(version_id,''),COALESCE(source_key,''),COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_evidence WHERE change_id=? AND sequence>? ORDER BY sequence LIMIT 100`, changeID, after)
	if err != nil {
		return nil, err
	}
	result := make([]coreChangeEvidenceRow, 0, 100)
	for rows.Next() {
		var row coreChangeEvidenceRow
		var encoded string
		if err := rows.Scan(&row.sequence, &row.kind, &row.versionID, &row.sourceKey, &encoded); err != nil {
			return nil, err
		}
		row.raw, err = readCoreChangePayload(ctx, tx, source, encoded)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, closeCoreChangeRows(rows)
}

func importCoreChangeVersions(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	var after int64
	for {
		rows, err := tx.QueryContext(ctx, `SELECT id,number,COALESCE(NULLIF(payload_object_key,''),record_json),policy_revision_id FROM change_versions WHERE change_id=? AND number>? ORDER BY number LIMIT 100`, changeID, after)
		if err != nil {
			return err
		}
		type versionRow struct {
			id, encoded, policyID string
			number                int64
		}
		page := make([]versionRow, 0, 100)
		for rows.Next() {
			var row versionRow
			if err := rows.Scan(&row.id, &row.number, &row.encoded, &row.policyID); err != nil {
				return err
			}
			page = append(page, row)
		}
		if err := closeCoreChangeRows(rows); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			raw, err := readCoreChangePayload(ctx, tx, source, row.encoded)
			if err != nil {
				return err
			}
			var value tracker.ChangeVersion
			if err := json.Unmarshal(raw, &value); err != nil || value.ID != row.id || value.ChangeID != changeID || value.Number != row.number || value.PolicyID != row.policyID {
				return errors.Join(eventstore.ErrInvalidRecord, err)
			}
			sourceCtx := coreChangeSourceContext(ctx, tx, source.SourceObjects)
			if err := hydrateChangeRecord(sourceCtx, tx, &value); err != nil {
				return fmt.Errorf("hydrate source version: %w", err)
			}
			bundle, err := readCoreChangeBundle(ctx, tx, source, value)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(value)
			if err != nil {
				return err
			}
			record, err := content.PrepareVersion(ctx, source.OrganizationID, payload)
			if err != nil {
				return err
			}
			if len(bundle) != 0 {
				ref, err := content.PrepareSourceBundle(ctx, source.OrganizationID, bundle)
				if err != nil {
					return err
				}
				record.SourceObjectKey, record.SourceSHA256, record.SourceBytes, record.SourceContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
			}
			receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change-version", row.id, struct {
				Version eventstore.Version `json:"version"`
				Source  []byte             `json:"source,omitempty"`
			}{record, bundle})
			if err != nil {
				return err
			}
			if err := target.WriteRecords(ctx, source.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
				replayed, err := checkCoreChangeReceipt(ctx, w, receipt)
				if err != nil || replayed {
					return err
				}
				if _, err := w.CreateVersion(ctx, record); err != nil {
					return err
				}
				_, err = w.PutReceipt(ctx, receipt)
				return err
			}); err != nil {
				return err
			}
			after = row.number
		}
	}
}

func importCoreChangePublications(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT p.version_id,p.change_id,p.repository,p.base_ref,p.branch,p.head_sha,p.policy_id,p.source_attempt_id,p.recorder_attempt_id,p.provider,p.external_id,p.external_url,COALESCE(NULLIF(v.payload_object_key,''),v.record_json)
FROM native_pr_publications p JOIN change_versions v ON v.id=p.version_id JOIN change_requests c ON c.id=p.change_id
WHERE c.organization_id=? AND p.change_id=? AND p.version_id>? ORDER BY p.version_id LIMIT 100`, source.OrganizationID, changeID, after)
		if err != nil {
			return err
		}
		type publicationRow struct {
			record eventstore.NativePRPublication
			raw    string
		}
		page := make([]publicationRow, 0, 100)
		for rows.Next() {
			var row publicationRow
			if err := rows.Scan(&row.record.VersionID, &row.record.ChangeID, &row.record.Repository, &row.record.BaseRef, &row.record.Branch, &row.record.HeadSHA, &row.record.PolicyID, &row.record.SourceAttemptID, &row.record.RecorderAttemptID, &row.record.External.Provider, &row.record.External.ID, &row.record.External.URL, &row.raw); err != nil {
				return err
			}
			row.record.OrganizationID = source.OrganizationID
			page = append(page, row)
		}
		if err := closeCoreChangeRows(rows); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			raw, err := readCoreChangePayload(ctx, tx, source, row.raw)
			if err != nil {
				return err
			}
			var version tracker.ChangeVersion
			publication := tracker.NativePRPublication{ChangeID: row.record.ChangeID, VersionID: row.record.VersionID, Repository: row.record.Repository, BaseRef: row.record.BaseRef, Branch: row.record.Branch, HeadSHA: row.record.HeadSHA, PolicyID: row.record.PolicyID, SourceVersion: tracker.NativeChangeReference{ChangeID: row.record.ChangeID, VersionID: row.record.VersionID, HeadSHA: row.record.HeadSHA}, SourceAttemptID: row.record.SourceAttemptID, External: row.record.External}
			if err := json.Unmarshal(raw, &version); err != nil || version.ID != row.record.VersionID || version.ChangeID != row.record.ChangeID || !publication.Matches(row.record.ChangeID, version) {
				return errors.Join(eventstore.ErrInvalidRecord, err)
			}
			receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change-publication", row.record.VersionID, row.record)
			if err != nil {
				return err
			}
			if err := target.ImportCoreNativePRPublication(ctx, row.record, receipt); err != nil {
				return err
			}
			after = row.record.VersionID
		}
	}
}

func importCoreReleaseDeliveries(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash string) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT project_id,work_item_id,source_issue_id,source_change_id,source_merge_sha,repository_id,repository,pr_number,head_ref,head_sha,base_ref,base_sha,frozen_sha,change_id,version_id,principal_id,actor_kind,via_key,associated_at
FROM release_deliveries WHERE organization_id=? AND work_item_id>? ORDER BY work_item_id LIMIT 100`, source.OrganizationID, after)
		if err != nil {
			return err
		}
		page := make([]eventstore.ReleaseDelivery, 0, 100)
		for rows.Next() {
			var record eventstore.ReleaseDelivery
			var repositoryID sql.NullInt64
			var versionID sql.NullString
			var associatedAt string
			if err := rows.Scan(&record.ProjectID, &record.WorkItemID, &record.SourceIssueID, &record.SourceChangeID, &record.SourceMergeSHA, &repositoryID, &record.Repository, &record.PRNumber, &record.HeadRef, &record.HeadSHA, &record.BaseRef, &record.BaseSHA, &record.FrozenSHA, &record.ChangeID, &versionID, &record.Actor.PrincipalID, &record.Actor.Kind, &record.Actor.ViaKey, &associatedAt); err != nil {
				return err
			}
			if repositoryID.Valid {
				record.RepositoryID = &repositoryID.Int64
			}
			record.VersionID = versionID.String
			record.AssociatedAt, err = parseTimeValue(associatedAt)
			if err != nil || record.AssociatedAt.IsZero() {
				return errors.Join(eventstore.ErrInvalidRecord, err)
			}
			record.OrganizationID = source.OrganizationID
			page = append(page, record)
		}
		if err := closeCoreChangeRows(rows); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, record := range page {
			if record.VersionID != "" {
				version, err := targetVersionForCoreImport(ctx, target, source.OrganizationID, record.ChangeID, record.VersionID)
				if err != nil || version.Repository != record.Repository || version.HeadSHA != record.HeadSHA {
					return errors.Join(eventstore.ErrInvalidRecord, err)
				}
			}
			receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-release-delivery", string(record.WorkItemID), record)
			if err != nil {
				return err
			}
			if err := target.ImportCoreReleaseDelivery(ctx, record, receipt); err != nil {
				return err
			}
			after = string(record.WorkItemID)
		}
	}
}

func targetVersionForCoreImport(ctx context.Context, target *postgres.Repository, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error) {
	var record eventstore.Version
	err := target.ReadRecords(ctx, organization, corestore.Background, func(reader eventstore.Reader) error {
		var err error
		record, err = reader.Version(ctx, changeID, versionID)
		return err
	})
	return record, err
}

func readCoreChangeBundle(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, value tracker.ChangeVersion) ([]byte, error) {
	var inline, key, digest string
	var compressed, content sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT bundle,COALESCE(object_key,''),COALESCE(object_sha256,''),object_bytes,content_bytes FROM change_sources WHERE version_id=?`, value.ID).Scan(&inline, &key, &digest, &compressed, &content)
	if errors.Is(err, sql.ErrNoRows) {
		if value.Source != nil {
			return nil, eventstore.ErrInvalidRecord
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bundle := []byte(inline)
	if key != "" {
		if !compressed.Valid || !content.Valid {
			return nil, eventstore.ErrInvalidRecord
		}
		metadata, err := tx.QueryContext(ctx, `SELECT object_key,object_sha256,object_bytes,content_bytes FROM change_content_objects WHERE organization_id=? AND object_key=?`, source.OrganizationID, key)
		if err != nil {
			return nil, err
		}
		defer metadata.Close()
		var ref attachment.ArchiveReference
		ref.Codec, ref.SchemaVersion, ref.Kind, ref.Retention = "gzip", 1, "archive", attachment.ArchiveRetention
		if !metadata.Next() {
			return nil, errors.Join(eventstore.ErrInvalidRecord, metadata.Err())
		}
		if err := metadata.Scan(&ref.Key, &ref.SHA256, &ref.CompressedSize, &ref.UncompressedSize); err != nil {
			return nil, err
		}
		if err := metadata.Err(); err != nil {
			return nil, err
		}
		if ref.Key != key || ref.SHA256 != digest || ref.CompressedSize != compressed.Int64 || ref.UncompressedSize != content.Int64 {
			return nil, eventstore.ErrInvalidRecord
		}
		archived, err := readCoreChangeObject(ctx, tx, source, key)
		if err != nil {
			return nil, err
		}
		if len(bundle) != 0 && !bytes.Equal(bundle, archived) {
			return nil, eventstore.ErrInvalidRecord
		}
		bundle = archived
	}
	if value.Source == nil {
		return nil, eventstore.ErrInvalidRecord
	}
	if value.Source.Validate(value.BaseSHA, value.HeadSHA, bundle) != nil {
		return nil, eventstore.ErrInvalidRecord
	}
	return bundle, nil
}

func importCoreChangeEvidence(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	var after int64
	for {
		page, err := readCoreChangeEvidencePage(ctx, tx, source, changeID, after)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			var versionID, sourceKey *string
			if row.versionID != "" {
				versionID = &row.versionID
			}
			if row.sourceKey != "" {
				sourceKey = &row.sourceKey
			}
			record, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: source.OrganizationID, ChangeID: changeID, Sequence: row.sequence, VersionID: versionID, Kind: row.kind, SourceKey: sourceKey}, row.raw)
			if err != nil {
				return err
			}
			if (record.SourceKey == nil) != (sourceKey == nil) || record.SourceKey != nil && *record.SourceKey != *sourceKey {
				return eventstore.ErrInvalidRecord
			}
			receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change-evidence", changeID+"/"+coreChangeEvidenceKey(row.sequence), record)
			if err != nil {
				return err
			}
			if err := target.WriteRecords(ctx, source.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
				replayed, err := checkCoreChangeReceipt(ctx, w, receipt)
				if err != nil || replayed {
					return err
				}
				if _, err := w.AppendEvidence(ctx, record); err != nil {
					return err
				}
				_, err = w.PutReceipt(ctx, receipt)
				return err
			}); err != nil {
				return err
			}
			after = row.sequence
		}
	}
}

func importCoreChangeLandings(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	type sourceLanding struct {
		attemptID  string
		versionID  string
		repository string
		raw        string
	}
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT l.attempt_id,l.version_id,v.repository,l.record_json FROM change_landing_receipts l JOIN change_versions v ON v.id=l.version_id WHERE v.change_id=? AND l.attempt_id>? ORDER BY l.attempt_id LIMIT 100`, changeID, after)
		if err != nil {
			return err
		}
		page := make([]sourceLanding, 0, 100)
		for rows.Next() {
			var row sourceLanding
			if err := rows.Scan(&row.attemptID, &row.versionID, &row.repository, &row.raw); err != nil {
				return err
			}
			page = append(page, row)
		}
		if err := closeCoreChangeRows(rows); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			var value tracker.NativeLandingReceipt
			if err := json.Unmarshal([]byte(row.raw), &value); err != nil || value.VersionID != row.versionID {
				return errors.Join(eventstore.ErrInvalidRecord, err)
			}
			record, err := content.PrepareLandingReceipt(ctx, eventstore.LandingReceipt{OrganizationID: source.OrganizationID, AttemptID: row.attemptID, VersionID: row.versionID, Repository: row.repository, BaseRef: value.BaseRef}, value)
			if err != nil {
				return err
			}
			receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change-landing", row.attemptID, record)
			if err != nil {
				return err
			}
			if err := target.WriteRecords(ctx, source.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
				replayed, err := checkCoreChangeReceipt(ctx, w, receipt)
				if err != nil || replayed {
					return err
				}
				if _, err := w.CreateLandingReceipt(ctx, record); err != nil {
					return err
				}
				_, err = w.PutReceipt(ctx, receipt)
				return err
			}); err != nil {
				return err
			}
			after = row.attemptID
		}
	}
}

func importCoreChangeViewedFiles(ctx context.Context, tx *sql.Tx, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	var afterVersion, afterPrincipal, afterManifest, afterFile string
	for {
		rows, err := tx.QueryContext(ctx, `SELECT f.version_id,f.principal_id,f.manifest_sha256,f.file_sha256,f.viewed,v.created_at,c.project_id
FROM change_viewed_files f JOIN change_versions v ON v.id=f.version_id JOIN change_requests c ON c.id=v.change_id
WHERE v.change_id=? AND (f.version_id>? OR (f.version_id=? AND f.principal_id>?) OR (f.version_id=? AND f.principal_id=? AND f.manifest_sha256>?) OR (f.version_id=? AND f.principal_id=? AND f.manifest_sha256=? AND f.file_sha256>?))
ORDER BY f.version_id,f.principal_id,f.manifest_sha256,f.file_sha256 LIMIT 100`, changeID, afterVersion, afterVersion, afterPrincipal, afterVersion, afterPrincipal, afterManifest, afterVersion, afterPrincipal, afterManifest, afterFile)
		if err != nil {
			return err
		}
		page := make([]eventstore.ViewedFile, 0, 100)
		for rows.Next() {
			var record eventstore.ViewedFile
			var createdAt string
			if err := rows.Scan(&record.VersionID, &record.PrincipalID, &record.ManifestSHA256, &record.FileSHA256, &record.Viewed, &createdAt, &record.ProjectID); err != nil {
				return err
			}
			record.UpdatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
			if err != nil || record.UpdatedAt.IsZero() {
				return errors.Join(eventstore.ErrInvalidRecord, err)
			}
			record.OrganizationID, record.ChangeID = source.OrganizationID, changeID
			page = append(page, record)
		}
		if err := closeCoreChangeRows(rows); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, record := range page {
			keyBytes, err := json.Marshal([4]string{record.VersionID, record.PrincipalID, record.ManifestSHA256, record.FileSHA256})
			if err != nil {
				return err
			}
			receipt, err := prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, copyHash, "core-change-viewed-file", string(keyBytes), record)
			if err != nil {
				return err
			}
			if err := target.WriteRecords(ctx, source.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
				replayed, err := checkCoreChangeReceipt(ctx, w, receipt)
				if err != nil || replayed {
					return err
				}
				if _, err := w.SetViewedFile(ctx, record); err != nil {
					return err
				}
				_, err = w.PutReceipt(ctx, receipt)
				return err
			}); err != nil {
				return err
			}
			afterVersion, afterPrincipal, afterManifest, afterFile = record.VersionID, record.PrincipalID, record.ManifestSHA256, record.FileSHA256
		}
	}
}

func coreChangeEvidenceKey(sequence int64) string { return strconv.FormatInt(sequence, 10) }
