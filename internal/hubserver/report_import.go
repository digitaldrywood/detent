package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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

func ImportCoreReportLandings(ctx context.Context, target *postgres.Repository, source CoreChangeImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return summary, errors.New("invalid report landing import source")
	}
	digest := sha256.New()
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
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
				if err := target.ImportReportChangeLanding(ctx, record, proof); err != nil {
					return err
				}
				encoded, err := json.Marshal(struct {
					ID      string
					Landing *tracker.ChangeLanding
				}{row.change.ID, row.change.Landed})
				if err != nil {
					return err
				}
				digest.Write(encoded)
				summary.Landings++
			}
			after = page[len(page)-1].change.ID
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}

func coreReportLandingProof(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, change tracker.ChangeRequest) (eventstore.LandingReceipt, error) {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT l.attempt_id,l.record_json,v.repository FROM change_landing_receipts l JOIN change_versions v ON v.id=l.version_id WHERE l.version_id=? AND l.attempt_id>? ORDER BY l.attempt_id LIMIT 100`, change.Landed.VersionID, after)
		if err != nil {
			return eventstore.LandingReceipt{}, err
		}
		type sourceRow struct{ id, raw, repository string }
		page := []sourceRow{}
		for rows.Next() {
			var row sourceRow
			if err := rows.Scan(&row.id, &row.raw, &row.repository); err != nil {
				rows.Close()
				return eventstore.LandingReceipt{}, err
			}
			page = append(page, row)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return eventstore.LandingReceipt{}, err
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

func restoreCoreReportLanding(ctx context.Context, tx *sql.Tx, source CoreChangeImportSource, change *tracker.ChangeRequest) error {
	if change.Landed != nil || change.CurrentVersion == "" {
		return nil
	}
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT l.attempt_id,l.record_json,coalesce(nullif(v.payload_object_key,''),v.record_json) FROM change_landing_receipts l JOIN change_versions v ON v.id=l.version_id WHERE l.version_id=? AND l.attempt_id>? ORDER BY l.attempt_id LIMIT 100`, change.CurrentVersion, after)
		if err != nil {
			return err
		}
		type receiptRow struct{ id, receipt, version string }
		page := []receiptRow{}
		for rows.Next() {
			var row receiptRow
			if err := rows.Scan(&row.id, &row.receipt, &row.version); err != nil {
				rows.Close()
				return err
			}
			page = append(page, row)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
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

func decodeCoreReportData(ctx context.Context, tx *sql.Tx, source CoreWorkItemImportSource, raw []byte, data *tracker.CollaborationData) error {
	objects := reportImportObjects{organization: source.OrganizationID, store: source.Objects}
	content := &changeContent{store: objects, query: tx, payloads: map[string][]byte{}, references: map[string]attachment.ArchiveReference{}}
	ctx = context.WithValue(ctx, changeContentKey{}, content)
	ctx = context.WithValue(ctx, runDetailContextKey{}, &runDetailContext{store: objects, organization: string(source.OrganizationID), cache: map[string]map[string]tracker.NativeRunData{}})
	ctx = payloadQueryContext(ctx, tx)
	if err := decodeStorageJSON(ctx, raw, data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := decodeStorageJSON(ctx, raw, &fields); err != nil {
		return err
	}
	if runRaw := fields["run"]; len(runRaw) > 0 && string(runRaw) != "null" {
		var run tracker.NativeRunData
		if err := decodeActivityRun(ctx, tx, string(runRaw), &run); err != nil {
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

func ImportCoreReportEvents(ctx context.Context, target *postgres.Repository, source CoreWorkItemImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.ProjectID == "" || source.Objects == nil {
		return summary, errors.New("invalid report import source")
	}
	digest := sha256.New()
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		summary.SourceSHA256 = copyHash
		after := ""
		for {
			rows, err := tx.QueryContext(ctx, `SELECT id,work_item_id,sequence,type,schema_version,actor_json,data_json,event_revision,recorded_at FROM collaboration_events WHERE organization_id=? AND project_id=? AND id>? AND (type LIKE 'run.%' OR type='workflow.transitioned') ORDER BY id LIMIT 100`, source.OrganizationID, source.ProjectID, after)
			if err != nil {
				return err
			}
			type row struct {
				id, item, kind, actor, data, at string
				sequence, schema, revision      int64
			}
			page := []row{}
			for rows.Next() {
				var value row
				if err := rows.Scan(&value.id, &value.item, &value.sequence, &value.kind, &value.schema, &value.actor, &value.data, &value.revision, &value.at); err != nil {
					rows.Close()
					return err
				}
				page = append(page, value)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(page) == 0 {
				return nil
			}
			for _, row := range page {
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
				event.EventRevision = row.revision

				if event.Report == nil {
					continue
				}
				if err := target.ImportReportEventProjection(ctx, event); err != nil {
					if !errors.Is(err, corestore.ErrRevisionConflict) {
						return fmt.Errorf("import report event projection: %w", err)
					}
					var legacyData tracker.CollaborationData
					if json.Unmarshal([]byte(row.data), &legacyData) != nil {
						return eventstore.ErrInvalidRecord
					}
					legacyValue := value
					legacyValue.Data = legacyData
					legacyRaw, encodeErr := json.Marshal(legacyValue)
					if encodeErr != nil {
						return encodeErr
					}
					legacy, prepareErr := eventstore.NewContent(source.Objects).PrepareEvent(ctx, source.OrganizationID, legacyRaw)
					if prepareErr != nil {
						return prepareErr
					}
					legacy.EventRevision, legacy.Report = row.revision, event.Report
					if importErr := target.ImportReportEventProjection(ctx, legacy); importErr != nil {
						return fmt.Errorf("match legacy report projection source: %w", importErr)
					}
					summary.LegacyAnchors++
				}
				proof, err := json.Marshal(struct {
					ID         string
					Projection *reportstore.EventProjection
				}{event.ID, event.Report})
				if err != nil {
					return err
				}
				digest.Write(proof)
				summary.Events++
				if event.AttemptID != "" {
					summary.AttemptObservations++
				}
				if event.Report.ReworkClass != "" {
					summary.Reworks++
				}
			}
			after = page[len(page)-1].id
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}

func ImportCoreReportEscapes(ctx context.Context, target *postgres.Repository, source CoreChangeImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.SourceObjects == nil || source.Objects == nil {
		return summary, errors.New("invalid report escape import source")
	}
	digest := sha256.New()
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		summary.SourceSHA256 = copyHash
		changeAfter := ""
		sequenceAfter := int64(0)
		for {
			rows, err := tx.QueryContext(ctx, `SELECT e.change_id,e.sequence,coalesce(e.version_id,''),coalesce(e.source_key,''),coalesce(nullif(e.payload_object_key,''),e.record_json) FROM change_evidence e JOIN change_requests c ON c.id=e.change_id WHERE c.organization_id=? AND e.kind='discussion' AND (e.change_id>? OR (e.change_id=? AND e.sequence>?)) ORDER BY e.change_id,e.sequence LIMIT 100`, source.OrganizationID, changeAfter, changeAfter, sequenceAfter)
			if err != nil {
				return err
			}
			type sourceRow struct {
				change, version, key, raw string
				sequence                  int64
			}
			page := []sourceRow{}
			for rows.Next() {
				var row sourceRow
				if err := rows.Scan(&row.change, &row.sequence, &row.version, &row.key, &row.raw); err != nil {
					rows.Close()
					return err
				}
				page = append(page, row)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
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
				if err := target.ImportReportEscapeProjection(ctx, record); err != nil {
					return err
				}
				proof, err := json.Marshal(struct {
					Change     string
					Sequence   int64
					Projection *reportstore.EscapeProjection
				}{row.change, row.sequence, record.ReportEscape})
				if err != nil {
					return err
				}
				digest.Write(proof)
				summary.Escapes++
			}
			last := page[len(page)-1]
			changeAfter, sequenceAfter = last.change, last.sequence
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}

type ReportUsageImportSource struct {
	CopyPath       string
	OrganizationID tracker.OrganizationID
	ExpectedSHA256 string
}

func ImportCoreReportUsage(ctx context.Context, target *postgres.Repository, source ReportUsageImportSource) (ReportImportSummary, error) {
	var summary ReportImportSummary
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || len(source.ExpectedSHA256) != 64 {
		return summary, errors.New("invalid pinned report usage source")
	}
	digest := sha256.New()
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if copyHash != source.ExpectedSHA256 {
			return errors.New("report usage source digest mismatch")
		}
		summary.SourceSHA256 = copyHash
		var invalid int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM attempt_usage u LEFT JOIN native_attempts a ON a.id=u.attempt_id WHERE a.id IS NULL OR u.organization_id<>? OR a.organization_id<>u.organization_id OR a.project_id<>u.project_id`, source.OrganizationID).Scan(&invalid); err != nil {
			return err
		}
		if invalid != 0 {
			return errors.New("report usage source has invalid attempt scope")
		}
		counts := map[string]int64{}
		var after int64
		for {
			rows, err := tx.QueryContext(ctx, `SELECT u.rowid,u.organization_id,u.project_id,a.work_item_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,u.updated_at FROM attempt_usage u JOIN native_attempts a ON a.id=u.attempt_id WHERE u.rowid>? ORDER BY u.rowid LIMIT 100`, after)
			if err != nil {
				return err
			}
			type entry struct {
				id     int64
				record reportstore.UsageRecord
			}
			page := []entry{}
			for rows.Next() {
				var e entry
				var period, updated string
				if err := rows.Scan(&e.id, &e.record.OrganizationID, &e.record.ProjectID, &e.record.WorkItemID, &e.record.AttemptID, &period, &e.record.Provider, &e.record.Model, &e.record.Input, &e.record.CachedInput, &e.record.Output, &e.record.Cost, &e.record.Currency, &updated); err != nil {
					rows.Close()
					return err
				}
				if e.record.OrganizationID != source.OrganizationID {
					rows.Close()
					return corestore.ErrTenantMismatch
				}
				e.record.Period, err = time.Parse(time.RFC3339Nano, period)
				if err != nil {
					rows.Close()
					return err
				}
				e.record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
				if err != nil {
					rows.Close()
					return err
				}
				page = append(page, e)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(page) == 0 {
				for attempt, count := range counts {
					if err := target.VerifyReportUsageImport(ctx, source.OrganizationID, attempt, copyHash, count); err != nil {
						return err
					}
				}
				return nil
			}
			for _, e := range page {
				if err := target.ImportReportUsageFact(ctx, e.record); err != nil {
					return err
				}
				encoded, err := json.Marshal(e.record)
				if err != nil {
					return err
				}
				digest.Write(encoded)
				summary.UsageRows++
				counts[e.record.AttemptID]++
			}
			after = page[len(page)-1].id
		}
	})
	summary.ProjectionSHA256 = hex.EncodeToString(digest.Sum(nil))
	return summary, err
}
