package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreProjectImportSource struct {
	BundlePath     string
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Objects        attachment.DetailStore
}

type coreProjectImportRow struct {
	Record          projectstore.Record
	Workflow        string
	Models          string
	States          string
	Created         string
	Deleted         sql.NullString
	RepositoryID    sql.NullInt64
	IconKind        sql.NullString
	IconName        sql.NullString
	IconColor       sql.NullString
	IconEmoji       sql.NullString
	IconImageKey    sql.NullString
	IconImageSHA256 sql.NullString
}

func (r *Repository) ImportCoreProjects(ctx context.Context, source CoreProjectImportSource) (int64, error) {
	if source.CopyPath != "" {
		return 0, neutralimport.ErrInvalid
	}

	if source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, errors.New("invalid core project import source")
	}

	return r.importNeutralCoreProjects(ctx, source)
}

func (r *Repository) PinCoreImportSource(ctx context.Context, organization tracker.OrganizationID, copyHash string, objects attachment.DetailStore) error {
	if organization == "" || !artifact.ValidHash(copyHash, 64) || objects == nil {
		return errors.New("invalid core import source pin")
	}
	pin, err := eventstore.NewContent(objects).PrepareReceipt(ctx, eventstore.Receipt{
		OrganizationID: organization,
		ActorID:        "production-import",
		Operation:      "core-project-source",
		CommandKey:     "source",
		RequestHash:    copyHash,
	}, json.RawMessage(`{}`))
	if err != nil {
		return err
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		previous, err := s.PutReceipt(ctx, pin)
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return mutation.ErrConflict
		}
		if err != nil {
			return err
		}
		if previous.RequestHash != pin.RequestHash {
			return mutation.ErrConflict
		}
		return nil
	})
}

func (r *Repository) importCoreProjectPages(ctx context.Context, source CoreProjectImportSource, read func(string) ([]coreProjectImportRow, error)) (int64, error) {
	content := eventstore.NewContent(source.Objects)
	var count int64
	var after string
	for {
		rows, err := read(after)
		if err != nil {
			return count, err
		}
		if len(rows) == 0 {
			return count, nil
		}

		for _, row := range rows {
			record, receipt, err := prepareCoreProjectImport(ctx, content, source, row)
			if err != nil {
				return count, err
			}
			err = r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				previous, err := s.Receipt(ctx, receipt.ActorID, receipt.Operation, receipt.CommandKey)
				if err == nil {
					if previous.RequestHash != receipt.RequestHash {
						return mutation.ErrConflict
					}
					return nil
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				if _, err := s.CreateProjectRecord(ctx, record); err != nil {
					return err
				}
				if _, err := s.db.Exec(ctx, `UPDATE projects SET created_at=$3,deleted_at=$4 WHERE organization_id=$1 AND id=$2`, source.OrganizationID, record.Core.ID, record.Core.CreatedAt, record.Core.DeletedAt); err != nil {
					return err
				}
				v := record.Settings
				if _, err := s.db.Exec(ctx, `UPDATE project_settings SET revision=$3,intake=$4,projection=$5,repository_enabled=$6,repository=$7,checkout_repository=$8,aging_enabled=$9,duplicate_challenge=$10,archive_completed_after_days=$11,archive_cancelled_after_days=$12,workflow_key=$13,workflow_sha256=$14,workflow_bytes=$15,workflow_content_bytes=$16,require_visual_attachments=$17,model_revision=$18,model_key=$19,model_sha256=$20,model_bytes=$21,model_content_bytes=$22 WHERE organization_id=$1 AND project_id=$2`, source.OrganizationID, record.Core.ID, v.Revision, v.Intake, v.Projection, v.RepositoryEnabled, v.Repository, v.CheckoutRepository, v.AgingEnabled, v.DuplicateChallenge, v.ArchiveCompletedAfterDays, v.ArchiveCancelledAfterDays, v.WorkflowKey, v.WorkflowSHA256, v.WorkflowBytes, v.WorkflowContentBytes, v.RequireVisualAttachments, v.ModelRevision, v.ModelKey, v.ModelSHA256, v.ModelBytes, v.ModelContentBytes); err != nil {
					return err
				}
				_, err = s.PutReceipt(ctx, receipt)
				return err
			})
			if err != nil {
				return count, fmt.Errorf("import core project: %w", err)
			}
			after = string(record.Core.ID)
			count++
		}
	}
}

func prepareCoreProjectImport(ctx context.Context, content *eventstore.Content, source CoreProjectImportSource, row coreProjectImportRow) (projectstore.Record, eventstore.Receipt, error) {
	record := row.Record
	if record.Core.OrganizationID != source.OrganizationID || record.Settings.OrganizationID != source.OrganizationID || record.Core.ID == "" {
		return record, eventstore.Receipt{}, corestore.ErrTenantMismatch
	}
	if row.RepositoryID.Valid && record.Settings.Repository == "" {
		return record, eventstore.Receipt{}, errors.New("missing imported project repository")
	}
	var err error
	record.Core.CreatedAt, err = time.Parse(time.RFC3339Nano, row.Created)
	if err != nil {
		return record, eventstore.Receipt{}, errors.New("invalid project creation timestamp")
	}
	if row.Deleted.Valid {
		deleted, err := time.Parse(time.RFC3339Nano, row.Deleted.String)
		if err != nil {
			return record, eventstore.Receipt{}, errors.New("invalid project deletion timestamp")
		}
		record.Core.DeletedAt = &deleted
	}
	if err := json.Unmarshal([]byte(row.States), &record.States); err != nil {
		return record, eventstore.Receipt{}, errors.New("invalid imported project states")
	}
	workflowMarkdown, changed, err := historicalCloudWorkflow(record, row.Workflow, record.States)
	if err != nil {
		return record, eventstore.Receipt{}, fmt.Errorf("import project workflow lanes: %w", err)
	}
	if changed {
		record.Settings.Revision++
	}
	normalizeImportedReporterStates(&record, row.Workflow, historicalStateLayoutKnown(row.States))
	collapseImportedDuplicateTransitions(record.States)
	if err := policy.ValidateStates(record.States); err != nil {
		return record, eventstore.Receipt{}, err
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return record, eventstore.Receipt{}, err
	}
	digest := sha256.Sum256(encoded)
	receipt := eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-project", CommandKey: string(record.Core.ID), RequestHash: hex.EncodeToString(digest[:])}
	receipt, err = content.PrepareReceipt(ctx, receipt, encoded)
	if err != nil {
		return record, receipt, err
	}
	workflow, err := json.Marshal(workflowMarkdown)
	if err != nil {
		return record, receipt, err
	}
	workflowRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, workflow)
	if err != nil {
		return record, receipt, err
	}
	record.Settings.WorkflowKey, record.Settings.WorkflowSHA256, record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes = workflowRef.PayloadObjectKey, workflowRef.PayloadSHA256, workflowRef.PayloadBytes, workflowRef.PayloadContentBytes
	record.Core.WorkflowObjectKey = &record.Settings.WorkflowKey
	if row.Models != "" {
		modelRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, json.RawMessage(row.Models))
		if err != nil {
			return record, receipt, err
		}
		record.Settings.ModelKey, record.Settings.ModelSHA256, record.Settings.ModelBytes, record.Settings.ModelContentBytes = modelRef.PayloadObjectKey, modelRef.PayloadSHA256, modelRef.PayloadBytes, modelRef.PayloadContentBytes
		record.Core.ModelSelectionObjectKey = &record.Settings.ModelKey
	}
	statesRef, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID}, json.RawMessage(row.States))
	if err != nil {
		return record, receipt, err
	}
	record.Core.StatesObjectKey = &statesRef.PayloadObjectKey
	return record, receipt, nil
}

func normalizeImportedReporterStates(record *projectstore.Record, workflow string, knownLayout bool) {
	if record.Core.Profile != "native" {
		return
	}
	if knownLayout {
		record.States = projectstore.HistoricalHostedStates(record.States)
	}
	previous := record.States
	normalized := projectstore.TriageStates(previous)
	position := slices.IndexFunc(previous, func(state tracker.NativeState) bool { return state.Name == "Triage" })
	if (position < 0 || len(previous[position].Transitions) == 0) && !slices.ContainsFunc(previous, func(state tracker.NativeState) bool {
		return state.Name == "Todo" || state.Name == "Backlog" || state.Name == "Cancelled"
	}) {
		fallback := ""
		for _, state := range previous {
			if state.Name == "Triage" || state.OperatorOnly {
				continue
			}
			if fallback == "" {
				fallback = state.Name
			}
			if !state.Terminal {
				fallback = state.Name
				break
			}
		}
		if fallback != "" {
			normalized[0].Transitions = []string{fallback}
		}
	}
	if position < 0 {
		record.States = normalized
	} else {
		record.States = slices.Clone(previous)
		record.States[position] = normalized[0]
	}
	expected := []tracker.NativeState{
		{Name: "Triage", Transitions: []string{"Todo"}},
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done"}},
		{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	if !knownLayout || record.Core.WorkflowSource != "" || workflow != "" || !reflect.DeepEqual(record.States, expected) {
		return
	}
	record.States[0].Transitions = []string{"Todo", "Backlog", "Cancelled"}
	for index := 1; index < len(record.States); index++ {
		if !record.States[index].Terminal {
			record.States[index].Transitions = append(slices.Clone(record.States[index].Transitions), "Cancelled")
		}
	}
	record.States = append(record.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}})
}

func collapseImportedDuplicateTransitions(states []tracker.NativeState) {
	for index, state := range states {
		seen := make(map[string]struct{}, len(state.Transitions))
		unique := make([]string, 0, len(state.Transitions))
		for _, target := range state.Transitions {
			if _, duplicate := seen[target]; duplicate {
				continue
			}
			seen[target] = struct{}{}
			unique = append(unique, target)
		}
		if len(unique) != len(state.Transitions) {
			states[index].Transitions = unique
		}
	}
}

func historicalCloudWorkflow(record projectstore.Record, markdown string, states []tracker.NativeState) (string, bool, error) {
	if record.Core.Profile != "native" || record.Core.WorkflowSource != "" || markdown == "" {
		return markdown, false, nil
	}
	order := make([]string, 0, len(states))
	for _, state := range states {
		order = append(order, state.Name)
	}
	upgraded, changed, err := workflowconfig.MigrateTrackerLanes([]byte(markdown), order...)
	return string(upgraded), changed, err
}

func historicalStateLayoutKnown(raw string) bool {
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.DisallowUnknownFields()
	var states []tracker.NativeState
	return decoder.Decode(&states) == nil
}
