package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ReportImportTarget interface {
	ImportReportChangeLandings(context.Context, []postgres.ReportLandingImport) error
	ImportReportEventProjections(context.Context, []postgres.ReportEventProjectionImport) (int, error)
	ImportReportEscapeProjections(context.Context, []eventstore.Evidence) error
	ImportReportUsageFacts(context.Context, []reportstore.UsageRecord) error
	VerifyReportUsageImports(context.Context, tracker.OrganizationID, string, []postgres.ReportUsageImportCoverage) error
}

type ReportImportSummary struct {
	Events              int64
	AttemptObservations int64
	Reworks             int64
	Escapes             int64
	LegacyAnchors       int64
	Landings            int64
	UsageRows           int64
	SourceSHA256        string
	ProjectionSHA256    string
}

func ImportCoreReportLandings(ctx context.Context, target ReportImportTarget, source CoreChangeImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if source.CopyPath != "" {
		return summary, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return summary, errors.New("invalid report landing import source")
	}
	digest := sha256.New()
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := validateCoreNeutralChangeImport(bundle); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		summary.SourceSHA256 = copyHash
		after := ""
		for {
			page, err := readCoreChangePage(ctx, tx, source, after)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				return nil
			}
			batch := make([]postgres.ReportLandingImport, 0, len(page))
			proofs := make([][]byte, 0, len(page))
			for _, row := range page {
				if row.change.Landed == nil {
					continue
				}
				proof, err := coreReportLandingProof(ctx, tx, source, row.change)
				if err != nil {
					return err
				}
				raw, err := json.Marshal(row.change)
				if err != nil {
					return err
				}
				record, err := eventstore.NewContent(source.Objects).PrepareChange(ctx, source.OrganizationID, raw)
				if err != nil {
					return err
				}
				encoded, err := json.Marshal(struct {
					ID      string
					Landing *tracker.ChangeLanding
				}{row.change.ID, row.change.Landed})
				if err != nil {
					return err
				}
				batch = append(batch, postgres.ReportLandingImport{Change: record, Receipt: proof})
				proofs = append(proofs, encoded)
			}
			if len(batch) > 0 {
				if err := target.ImportReportChangeLandings(ctx, batch); err != nil {
					return err
				}
				for _, proof := range proofs {
					digest.Write(proof)
					summary.Landings++
				}
			}
			after = page[len(page)-1].change.ID
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}

func coreReportLandingProof(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, change tracker.ChangeRequest) (eventstore.LandingReceipt, error) {
	after := ""
	for {
		rows, err := tx.rows(ctx, "change_landing_receipts", map[string]string{"version_id": change.Landed.VersionID}, []string{"attempt_id"}, []string{after}, 100)
		if err != nil {
			return eventstore.LandingReceipt{}, err
		}
		type sourceRow struct{ id, raw, repository string }
		page := []sourceRow{}
		for _, sourceRecord := range rows {
			var row sourceRow
			if err := sourceRecord.Decode(map[string]any{"attempt_id": &row.id, "record_json": &row.raw}); err != nil {
				return eventstore.LandingReceipt{}, err
			}
			version, err := tx.one(ctx, "change_versions", map[string]string{"id": change.Landed.VersionID, "change_id": change.ID}, []string{"id"})
			if err != nil {
				return eventstore.LandingReceipt{}, err
			}
			if err := version.Decode(map[string]any{"repository": &row.repository}); err != nil {
				return eventstore.LandingReceipt{}, err
			}
			page = append(page, row)
		}
		if len(page) == 0 {
			return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
		}
		for _, row := range page {
			var receipt tracker.NativeLandingReceipt
			if json.Unmarshal([]byte(row.raw), &receipt) != nil {
				return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
			}
			if !receipt.Landed || receipt.VersionID != change.Landed.VersionID || receipt.HeadSHA != change.Landed.HeadSHA || receipt.MergeSHA != change.Landed.MergeSHA || receipt.BaseRef != change.Landed.BaseRef {
				continue
			}
			return eventstore.NewContent(source.Objects).PrepareLandingReceipt(ctx, eventstore.LandingReceipt{OrganizationID: source.OrganizationID, AttemptID: row.id, VersionID: receipt.VersionID, Repository: row.repository, BaseRef: receipt.BaseRef}, receipt)
		}
		after = page[len(page)-1].id
	}
}

func restoreCoreReportLanding(ctx context.Context, tx *coreNeutralImport, source CoreChangeImportSource, change *tracker.ChangeRequest) error {
	if change.Landed != nil || change.CurrentVersion == "" {
		return nil
	}
	after := ""
	for {
		rows, err := tx.rows(ctx, "change_landing_receipts", map[string]string{"version_id": change.CurrentVersion}, []string{"attempt_id"}, []string{after}, 100)
		if err != nil {
			return err
		}
		type receiptRow struct{ id, receipt, version string }
		page := []receiptRow{}
		for _, sourceRecord := range rows {
			var row receiptRow
			if err := sourceRecord.Decode(map[string]any{"attempt_id": &row.id, "record_json": &row.receipt}); err != nil {
				return err
			}
			version, err := tx.one(ctx, "change_versions", map[string]string{"id": change.CurrentVersion, "change_id": change.ID}, []string{"id"})
			if err != nil {
				return err
			}
			row.version, err = coreNeutralPayload(version)
			if err != nil {
				return err
			}
			page = append(page, row)
		}
		if len(page) == 0 {
			return nil
		}
		for _, row := range page {
			raw, err := readCoreChangePayload(ctx, tx, source, row.version)
			if err != nil {
				return err
			}
			var version tracker.ChangeVersion
			var receipt tracker.NativeLandingReceipt
			if json.Unmarshal(raw, &version) != nil || json.Unmarshal([]byte(row.receipt), &receipt) != nil || version.ID != change.CurrentVersion || version.ChangeID != change.ID || receipt.VersionID != version.ID {
				return eventstore.ErrInvalidRecord
			}
			if !receipt.Landed {
				continue
			}
			if receipt.HeadSHA != version.HeadSHA || receipt.MergeSHA == "" || receipt.BaseRef == "" || receipt.ObservedAt.IsZero() {
				return eventstore.ErrInvalidRecord
			}
			if prior := change.Landed; prior != nil && (prior.HeadSHA != receipt.HeadSHA || prior.MergeSHA != receipt.MergeSHA || prior.BaseRef != receipt.BaseRef) {
				return eventstore.ErrInvalidRecord
			}
			if change.Landed == nil || receipt.ObservedAt.Before(change.Landed.LandedAt) {
				change.Landed = &tracker.ChangeLanding{VersionID: version.ID, HeadSHA: receipt.HeadSHA, MergeSHA: receipt.MergeSHA, BaseRef: receipt.BaseRef, Method: receipt.Method, LandedAt: receipt.ObservedAt}
			}
		}
		after = page[len(page)-1].id
	}
}

func decodeCoreReportData(ctx context.Context, tx *coreNeutralImport, source CoreWorkItemImportSource, raw []byte, data *tracker.CollaborationData) error {
	objects := reportImportObjects{organization: source.OrganizationID, store: source.Objects}
	content := &changeContent{store: objects, payloads: map[string][]byte{}, references: map[string]attachment.ArchiveReference{}}
	owner := &runDetailContext{store: objects, organization: string(source.OrganizationID), cache: map[string]map[string]tracker.NativeRunData{}}
	ctx = context.WithValue(ctx, changeContentKey{}, content)
	ctx = context.WithValue(ctx, runDetailContextKey{}, owner)
	resolved, err := resolveCoreNeutralJSON(ctx, tx, source.OrganizationID, objects, raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resolved, data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(resolved, &fields); err != nil {
		return err
	}
	if runRaw := fields["run"]; len(runRaw) > 0 && string(runRaw) != "null" {
		resolved, err := resolveCoreNeutralJSON(ctx, tx, source.OrganizationID, objects, runRaw)
		if err != nil {
			return err
		}
		if err := prepareCoreNeutralRunDetail(ctx, tx, source.OrganizationID, objects, resolved, content, owner); err != nil {
			return err
		}
		var run tracker.NativeRunData
		if err := decodeActivityRun(ctx, nil, string(resolved), &run); err != nil {
			return err
		}
		data.Run = &run
	}
	return nil
}

type reportImportObjects struct {
	organization tracker.OrganizationID
	store        attachment.DetailStore
}

func (s reportImportObjects) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if organization != string(s.organization) {
		return attachment.ArchiveReference{}, artifact.ErrIntegrity
	}
	return s.store.StoreDetail(ctx, organization, raw)
}

func (s reportImportObjects) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if organization != string(s.organization) {
		return nil, artifact.ErrIntegrity
	}
	return s.store.ReadDetail(ctx, organization, ref)
}

func ImportCoreReportEvents(ctx context.Context, target ReportImportTarget, source CoreWorkItemImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if source.CopyPath != "" {
		return summary, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.ProjectID == "" || source.Objects == nil {
		return summary, errors.New("invalid report import source")
	}
	digest := sha256.New()
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := postgres.ValidateNeutralCoreImportTables(bundle, "collaboration_events", "change_content_objects", "attachments"); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		summary.SourceSHA256 = copyHash
		after := ""
		for {
			rows, err := tx.rows(ctx, "collaboration_events", map[string]string{"organization_id": string(source.OrganizationID), "project_id": string(source.ProjectID)}, []string{"id"}, []string{after}, 100)
			if err != nil {
				return err
			}
			type row struct {
				id, item, kind, actor, data, at string
				sequence, schema                int64
				revision                        sql.NullInt64
			}
			page := []row{}
			for _, raw := range rows {
				var value row
				if err := raw.Decode(map[string]any{"id": &value.id, "work_item_id": &value.item, "sequence": &value.sequence, "type": &value.kind, "schema_version": &value.schema, "actor_json": &value.actor, "data_json": &value.data, "event_revision": &value.revision, "recorded_at": &value.at}); err != nil {
					return err
				}
				page = append(page, value)
			}
			if len(page) == 0 {
				return nil
			}
			batch := make([]postgres.ReportEventProjectionImport, 0, len(page))
			sourceRows := make([]row, 0, len(page))
			proofs := make([][]byte, 0, len(page))
			observations, reworks := 0, 0
			for _, row := range page {
				if !strings.HasPrefix(row.kind, "run.") && row.kind != "workflow.transitioned" {
					continue
				}
				var actor tracker.Actor
				var data tracker.CollaborationData
				if json.Unmarshal([]byte(row.actor), &actor) != nil {
					return eventstore.ErrInvalidRecord
				}
				if err := decodeCoreReportData(ctx, tx, source, []byte(row.data), &data); err != nil {
					return err
				}
				at, err := time.Parse(time.RFC3339Nano, row.at)
				if err != nil {
					return err
				}
				value := tracker.CollaborationEvent{ID: row.id, OrganizationID: source.OrganizationID, ProjectID: source.ProjectID, AggregateType: "work_item", AggregateID: tracker.NativeWorkItemID(row.item), AggregateSequence: row.sequence, Type: row.kind, SchemaVersion: int(row.schema), Actor: actor, Data: data, RecordedAt: at}
				raw, err := json.Marshal(value)
				if err != nil {
					return err
				}
				event, err := eventstore.NewContent(source.Objects).PrepareEvent(ctx, source.OrganizationID, raw)
				if err != nil {
					return err
				}
				if row.revision.Valid {
					event.EventRevision = row.revision.Int64
				}

				if event.Report == nil {
					continue
				}
				batch = append(batch, postgres.ReportEventProjectionImport{Event: event})
				sourceRows = append(sourceRows, row)
				proof, err := json.Marshal(struct {
					ID         string
					Projection *reportstore.EventProjection
				}{event.ID, event.Report})
				if err != nil {
					return err
				}
				proofs = append(proofs, proof)
				if event.AttemptID != "" {
					observations++
				}
				if event.Report.ReworkClass != "" {
					reworks++
				}
			}
			if len(batch) > 0 {
				legacyAnchors, importErr := target.ImportReportEventProjections(ctx, batch)
				if errors.Is(importErr, corestore.ErrRevisionConflict) {
					for i, record := range batch {
						var legacyData tracker.CollaborationData
						sourceRow := sourceRows[i]
						if err := json.Unmarshal([]byte(sourceRow.data), &legacyData); err != nil {
							return eventstore.ErrInvalidRecord
						}
						legacyValue := tracker.CollaborationEvent{ID: sourceRow.id, OrganizationID: source.OrganizationID, ProjectID: source.ProjectID, AggregateType: "work_item", AggregateID: tracker.NativeWorkItemID(sourceRow.item), AggregateSequence: sourceRow.sequence, Type: sourceRow.kind, SchemaVersion: int(sourceRow.schema), Actor: tracker.Actor{}, Data: legacyData}
						if err := json.Unmarshal([]byte(sourceRow.actor), &legacyValue.Actor); err != nil {
							return eventstore.ErrInvalidRecord
						}
						legacyValue.RecordedAt, err = time.Parse(time.RFC3339Nano, sourceRow.at)
						if err != nil {
							return err
						}
						legacyRaw, err := json.Marshal(legacyValue)
						if err != nil {
							return err
						}
						legacy, err := eventstore.NewContent(source.Objects).PrepareEvent(ctx, source.OrganizationID, legacyRaw)
						if err != nil {
							return err
						}
						legacy.EventRevision, legacy.Report = record.Event.EventRevision, record.Event.Report
						batch[i].Legacy = &legacy
					}
					legacyAnchors, importErr = target.ImportReportEventProjections(ctx, batch)
				}
				if importErr != nil {
					return fmt.Errorf("import report event projection batch: %w", importErr)
				}
				summary.LegacyAnchors += int64(legacyAnchors)
				for _, proof := range proofs {
					digest.Write(proof)
				}
				summary.Events += int64(len(batch))
				summary.AttemptObservations += int64(observations)
				summary.Reworks += int64(reworks)
			}
			after = page[len(page)-1].id
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}

func ImportCoreReportEscapes(ctx context.Context, target ReportImportTarget, source CoreChangeImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if source.CopyPath != "" {
		return summary, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return summary, errors.New("invalid report escape import source")
	}
	digest := sha256.New()
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := validateCoreNeutralChangeImport(bundle); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		summary.SourceSHA256 = copyHash
		changeAfter := ""
		sequenceAfter := int64(0)
		for {
			rows, err := tx.rows(ctx, "change_evidence", map[string]string{"kind": "discussion"}, []string{"change_id", "sequence"}, []string{changeAfter, strconv.FormatInt(sequenceAfter, 10)}, 100)
			if err != nil {
				return err
			}
			type sourceRow struct {
				change, version, key, raw string
				sequence                  int64
			}
			page := []sourceRow{}
			for _, sourceRecord := range rows {
				var row sourceRow
				if err := sourceRecord.Decode(map[string]any{"change_id": &row.change, "sequence": &row.sequence}); err != nil {
					return err
				}
				var version, key sql.NullString
				if err := sourceRecord.Decode(map[string]any{"version_id": &version, "source_key": &key}); err != nil {
					return err
				}
				row.version, row.key = version.String, key.String
				row.raw, err = coreNeutralPayload(sourceRecord)
				if err != nil {
					return err
				}
				if _, err := tx.one(ctx, "change_requests", map[string]string{"id": row.change, "organization_id": string(source.OrganizationID)}, []string{"id"}); err != nil {
					return err
				}
				page = append(page, row)
			}
			if len(page) == 0 {
				return nil
			}
			batch := make([]eventstore.Evidence, 0, len(page))
			proofs := make([][]byte, 0, len(page))
			for _, row := range page {
				raw, err := readCoreChangePayload(ctx, tx, source, row.raw)
				if err != nil {
					return err
				}
				record := eventstore.Evidence{OrganizationID: source.OrganizationID, ChangeID: row.change, Sequence: row.sequence, Kind: "discussion"}
				if row.version != "" {
					version := row.version
					record.VersionID = &version
				}
				if row.key != "" {
					key := row.key
					record.SourceKey = &key
				}
				record, err = eventstore.NewContent(source.Objects).PrepareEvidence(ctx, record, raw)
				if err != nil {
					return err
				}
				if record.ReportEscape == nil {
					continue
				}
				proof, err := json.Marshal(struct {
					Change     string
					Sequence   int64
					Projection *reportstore.EscapeProjection
				}{row.change, row.sequence, record.ReportEscape})
				if err != nil {
					return err
				}
				batch = append(batch, record)
				proofs = append(proofs, proof)
			}
			if len(batch) > 0 {
				if err := target.ImportReportEscapeProjections(ctx, batch); err != nil {
					return err
				}
				for _, proof := range proofs {
					digest.Write(proof)
				}
				summary.Escapes += int64(len(batch))
			}
			last := page[len(page)-1]
			changeAfter, sequenceAfter = last.change, last.sequence
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}

type ReportUsageImportSource struct {
	BundlePath     string
	CopyPath       string
	OrganizationID tracker.OrganizationID
	ExpectedSHA256 string
}

func ImportCoreReportUsage(ctx context.Context, target ReportImportTarget, source ReportUsageImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if source.CopyPath != "" {
		return summary, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || len(source.ExpectedSHA256) != 64 {
		return summary, errors.New("invalid pinned report usage source")
	}
	digest := sha256.New()
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := postgres.ValidateNeutralCoreImportTables(bundle, "attempt_usage", "native_attempts"); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		if copyHash != source.ExpectedSHA256 {
			return errors.New("report usage source digest mismatch")
		}
		summary.SourceSHA256 = copyHash
		if err := validateCoreNeutralUsageScope(ctx, tx, source.OrganizationID); err != nil {
			return err
		}
		counts := map[string]int64{}
		var after int64
		for {
			rows, err := bundle.OrdinalPage(ctx, "attempt_usage", after, 100)
			if err != nil {
				return err
			}
			type entry struct {
				id     int64
				record reportstore.UsageRecord
			}
			page := []entry{}
			for _, sourceRecord := range rows {
				var e entry
				var period, updated string
				if err := sourceRecord.Decode(map[string]any{"organization_id": &e.record.OrganizationID, "project_id": &e.record.ProjectID, "attempt_id": &e.record.AttemptID, "period": &period, "provider": &e.record.Provider, "model": &e.record.Model, "input": &e.record.Input, "cached_input": &e.record.CachedInput, "output": &e.record.Output, "cost_estimate": &e.record.Cost, "currency": &e.record.Currency, "updated_at": &updated}); err != nil {
					return err
				}
				e.id = sourceRecord.Ordinal
				attempt, err := tx.one(ctx, "native_attempts", map[string]string{"id": e.record.AttemptID, "organization_id": string(e.record.OrganizationID), "project_id": string(e.record.ProjectID)}, []string{"id"})
				if err != nil {
					return err
				}
				if err := attempt.Decode(map[string]any{"work_item_id": &e.record.WorkItemID}); err != nil {
					return err
				}
				if e.record.OrganizationID != source.OrganizationID {
					return corestore.ErrTenantMismatch
				}
				e.record.Period, err = time.Parse(time.RFC3339Nano, period)
				if err != nil {
					return err
				}
				e.record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
				if err != nil {
					return err
				}
				page = append(page, e)
			}
			if len(page) == 0 {
				break
			}
			batch := make([]reportstore.UsageRecord, 0, len(page))
			for _, e := range page {
				batch = append(batch, e.record)
				encoded, err := json.Marshal(e.record)
				if err != nil {
					return err
				}
				digest.Write(encoded)
				summary.UsageRows++
				counts[e.record.AttemptID]++
			}
			if err := target.ImportReportUsageFacts(ctx, batch); err != nil {
				return err
			}
			after = page[len(page)-1].id
		}
		attempts := make([]string, 0, len(counts))
		for attempt := range counts {
			attempts = append(attempts, attempt)
		}
		sort.Strings(attempts)
		for start := 0; start < len(attempts); start += 100 {
			end := start + 100
			if end > len(attempts) {
				end = len(attempts)
			}
			coverage := make([]postgres.ReportUsageImportCoverage, 0, end-start)
			for _, attempt := range attempts[start:end] {
				coverage = append(coverage, postgres.ReportUsageImportCoverage{AttemptID: attempt, RowCount: counts[attempt]})
			}
			if err := target.VerifyReportUsageImports(ctx, source.OrganizationID, copyHash, coverage); err != nil {
				return err
			}
		}
		return nil
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}
