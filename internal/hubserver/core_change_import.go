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
	BundlePath     string
	CopyPath       string
	OrganizationID tracker.OrganizationID
	SourceObjects  attachment.DetailStore
	Objects        attachment.DetailStore
}

func ImportCoreChanges(ctx context.Context, target *postgres.Repository, source CoreChangeImportSource) (int64, error) {
	if source.CopyPath != "" {
		return 0, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return 0, errors.New("invalid core change import source")
	}
	content := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := validateCoreNeutralChangeImport(bundle); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
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

func readCoreChangePage(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, after string) ([]coreChangeImportRow, error) {
	organization := source.OrganizationID
	rows, err := tx.rows(ctx, "change_requests", map[string]string{"organization_id": string(organization)}, []string{"id"}, []string{after}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreChangeImportRow, 0, 100)
	for _, sourceRecord := range rows {
		var id, project, item string
		var raw string
		if err := sourceRecord.Decode(map[string]any{"id": &id, "project_id": &project, "work_item_id": &item, "record_json": &raw}); err != nil {
			return nil, err
		}
		var change tracker.ChangeRequest
		resolved, err := resolveCoreNeutralJSON(ctx, tx, organization, source.SourceObjects, []byte(raw))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(resolved, &change); err != nil || change.ID != id || change.OrganizationID != organization || change.ProjectID != tracker.ProjectID(project) || change.WorkItemID != tracker.NativeWorkItemID(item) || change.Revision < 1 || change.CreatedAt.IsZero() || change.UpdatedAt.IsZero() {
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
	return result, nil
}

func readCoreChangeLinks(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, changeID, project string) ([]tracker.NativeWorkItemID, error) {
	rows, err := tx.rows(ctx, "change_issue_links", map[string]string{"organization_id": string(organization), "change_id": changeID}, []string{"work_item_id"}, nil, 33)
	if err != nil {
		return nil, err
	}
	links := make([]tracker.NativeWorkItemID, 0, 32)
	for _, sourceRecord := range rows {
		var linkProject, item string
		if err := sourceRecord.Decode(map[string]any{"project_id": &linkProject, "work_item_id": &item}); err != nil {
			return nil, err
		}
		if linkProject != project || item == "" || len(links) == 32 {
			return nil, eventstore.ErrInvalidRecord
		}
		links = append(links, tracker.NativeWorkItemID(item))
	}
	if len(links) == 0 {
		return nil, eventstore.ErrInvalidRecord
	}
	return links, nil
}

func importCoreChange(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash string, row coreChangeImportRow) error {
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

func readCoreChangeObject(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, key string) ([]byte, error) {
	return readCoreNeutralObject(ctx, tx, source.OrganizationID, source.SourceObjects, key)
}

func readCoreChangePayload(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, raw string) ([]byte, error) {
	if raw == "" {
		return nil, eventstore.ErrInvalidRecord
	}
	if json.Valid([]byte(raw)) {
		return resolveCoreNeutralJSON(ctx, tx, source.OrganizationID, source.SourceObjects, []byte(raw))
	}
	return readCoreChangeObject(ctx, tx, source, raw)
}

type coreChangeEvidenceRow struct {
	sequence                   int64
	kind, versionID, sourceKey string
	raw                        []byte
}

func readCoreChangeEvidencePage(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, changeID string, after int64) ([]coreChangeEvidenceRow, error) {
	rows, err := tx.rows(ctx, "change_evidence", map[string]string{"change_id": changeID}, []string{"sequence"}, []string{strconv.FormatInt(after, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreChangeEvidenceRow, 0, 100)
	for _, sourceRecord := range rows {
		var row coreChangeEvidenceRow
		var encoded string
		if err := sourceRecord.Decode(map[string]any{"sequence": &row.sequence, "kind": &row.kind}); err != nil {
			return nil, err
		}
		var version, key sql.NullString
		if err := sourceRecord.Decode(map[string]any{"version_id": &version, "source_key": &key}); err != nil {
			return nil, err
		}
		row.versionID, row.sourceKey = version.String, key.String
		encoded, err = coreNeutralPayload(sourceRecord)
		if err != nil {
			return nil, err
		}
		row.raw, err = readCoreChangePayload(ctx, tx, source, encoded)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, nil
}

func importCoreChangeVersions(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	var after int64
	for {
		rows, err := tx.rows(ctx, "change_versions", map[string]string{"change_id": changeID}, []string{"number"}, []string{strconv.FormatInt(after, 10)}, 100)
		if err != nil {
			return err
		}
		type versionRow struct {
			id, encoded, policyID string
			number                int64
		}
		page := make([]versionRow, 0, 100)
		for _, sourceRecord := range rows {
			var row versionRow
			if err := sourceRecord.Decode(map[string]any{"id": &row.id, "number": &row.number, "policy_revision_id": &row.policyID}); err != nil {
				return err
			}
			row.encoded, err = coreNeutralPayload(sourceRecord)
			if err != nil {
				return err
			}
			page = append(page, row)
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
			if err := hydrateCoreNeutralVersion(ctx, tx, source, &value); err != nil {
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

func importCoreChangePublications(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	after := ""
	for {
		rows, err := tx.rows(ctx, "native_pr_publications", map[string]string{"change_id": changeID}, []string{"version_id"}, []string{after}, 100)
		if err != nil {
			return err
		}
		type publicationRow struct {
			record eventstore.NativePRPublication
			raw    string
		}
		page := make([]publicationRow, 0, 100)
		for _, sourceRecord := range rows {
			var row publicationRow
			if err := sourceRecord.Decode(map[string]any{"version_id": &row.record.VersionID, "change_id": &row.record.ChangeID, "repository": &row.record.Repository, "base_ref": &row.record.BaseRef, "branch": &row.record.Branch, "head_sha": &row.record.HeadSHA, "policy_id": &row.record.PolicyID, "source_attempt_id": &row.record.SourceAttemptID, "recorder_attempt_id": &row.record.RecorderAttemptID, "provider": &row.record.External.Provider, "external_id": &row.record.External.ID, "external_url": &row.record.External.URL}); err != nil {
				return err
			}
			version, err := tx.one(ctx, "change_versions", map[string]string{"id": row.record.VersionID, "change_id": changeID}, []string{"id"})
			if err != nil {
				return err
			}
			row.raw, err = coreNeutralPayload(version)
			if err != nil {
				return err
			}
			row.record.OrganizationID = source.OrganizationID
			page = append(page, row)
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

func importCoreReleaseDeliveries(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash string) error {
	after := ""
	for {
		rows, err := tx.rows(ctx, "release_deliveries", map[string]string{"organization_id": string(source.OrganizationID)}, []string{"work_item_id"}, []string{after}, 100)
		if err != nil {
			return err
		}
		page := make([]eventstore.ReleaseDelivery, 0, 100)
		for _, sourceRecord := range rows {
			var record eventstore.ReleaseDelivery
			var repositoryID sql.NullInt64
			var versionID sql.NullString
			var associatedAt string
			if err := sourceRecord.Decode(map[string]any{"project_id": &record.ProjectID, "work_item_id": &record.WorkItemID, "source_issue_id": &record.SourceIssueID, "source_change_id": &record.SourceChangeID, "source_merge_sha": &record.SourceMergeSHA, "repository_id": &repositoryID, "repository": &record.Repository, "pr_number": &record.PRNumber, "head_ref": &record.HeadRef, "head_sha": &record.HeadSHA, "base_ref": &record.BaseRef, "base_sha": &record.BaseSHA, "frozen_sha": &record.FrozenSHA, "change_id": &record.ChangeID, "version_id": &versionID, "principal_id": &record.Actor.PrincipalID, "actor_kind": &record.Actor.Kind, "via_key": &record.Actor.ViaKey, "associated_at": &associatedAt}); err != nil {
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

func readCoreChangeBundle(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, value tracker.ChangeVersion) ([]byte, error) {
	row, err := tx.one(ctx, "change_sources", map[string]string{"version_id": value.ID}, []string{"version_id"})
	if errors.Is(err, sql.ErrNoRows) {
		if value.Source != nil {
			return nil, eventstore.ErrInvalidRecord
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	bundle, err := coreNeutralBytes(row, "bundle")
	if err != nil {
		return nil, err
	}
	var key, digest string
	var compressed, content sql.NullInt64
	if err := row.Decode(map[string]any{"object_key": &key, "object_sha256": &digest, "object_bytes": &compressed, "content_bytes": &content}); err != nil {
		return nil, err
	}
	if key != "" {
		if !compressed.Valid || !content.Valid {
			return nil, eventstore.ErrInvalidRecord
		}
		metadata, err := tx.one(ctx, "change_content_objects", map[string]string{"organization_id": string(source.OrganizationID), "object_key": key}, []string{"object_key"})
		if err != nil {
			return nil, err
		}
		ref := contentReference()
		if err := metadata.Decode(map[string]any{"object_key": &ref.Key, "object_sha256": &ref.SHA256, "object_bytes": &ref.CompressedSize, "content_bytes": &ref.UncompressedSize}); err != nil {
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
	if value.Source == nil || value.Source.Validate(value.BaseSHA, value.HeadSHA, bundle) != nil {
		return nil, eventstore.ErrInvalidRecord
	}
	return bundle, nil
}

func importCoreChangeEvidence(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
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

func importCoreChangeLandings(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	type sourceLanding struct {
		attemptID  string
		versionID  string
		repository string
		raw        string
	}
	after := ""
	for {
		rows, err := tx.rows(ctx, "change_landing_receipts", map[string]string{"change_id": changeID}, []string{"attempt_id"}, []string{after}, 100)
		if err != nil {
			return err
		}
		page := make([]sourceLanding, 0, 100)
		for _, sourceRecord := range rows {
			var row sourceLanding
			if err := sourceRecord.Decode(map[string]any{"attempt_id": &row.attemptID, "version_id": &row.versionID, "record_json": &row.raw}); err != nil {
				return err
			}
			version, err := tx.one(ctx, "change_versions", map[string]string{"id": row.versionID, "change_id": changeID}, []string{"id"})
			if err != nil {
				return err
			}
			if err := version.Decode(map[string]any{"repository": &row.repository}); err != nil {
				return err
			}
			page = append(page, row)
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

func importCoreChangeViewedFiles(ctx context.Context, tx *coreNeutralImport, target *postgres.Repository, content *eventstore.Content, source CoreChangeImportSource, copyHash, changeID string) error {
	var afterVersion, afterPrincipal, afterManifest, afterFile string
	for {
		rows, err := tx.rows(ctx, "change_viewed_files", map[string]string{"change_id": changeID}, []string{"version_id", "principal_id", "manifest_sha256", "file_sha256"}, []string{afterVersion, afterPrincipal, afterManifest, afterFile}, 100)
		if err != nil {
			return err
		}
		page := make([]eventstore.ViewedFile, 0, 100)
		for _, sourceRecord := range rows {
			var record eventstore.ViewedFile
			var createdAt string
			if err := sourceRecord.Decode(map[string]any{"version_id": &record.VersionID, "principal_id": &record.PrincipalID, "manifest_sha256": &record.ManifestSHA256, "file_sha256": &record.FileSHA256, "viewed": &record.Viewed}); err != nil {
				return err
			}
			version, err := tx.one(ctx, "change_versions", map[string]string{"id": record.VersionID, "change_id": changeID}, []string{"id"})
			if err != nil {
				return err
			}
			if err := version.Decode(map[string]any{"created_at": &createdAt}); err != nil {
				return err
			}
			change, err := tx.one(ctx, "change_requests", map[string]string{"id": changeID, "organization_id": string(source.OrganizationID)}, []string{"id"})
			if err != nil {
				return err
			}
			if err := change.Decode(map[string]any{"project_id": &record.ProjectID}); err != nil {
				return err
			}
			record.UpdatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
			if err != nil || record.UpdatedAt.IsZero() {
				return errors.Join(eventstore.ErrInvalidRecord, err)
			}
			record.OrganizationID, record.ChangeID = source.OrganizationID, changeID
			page = append(page, record)
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
