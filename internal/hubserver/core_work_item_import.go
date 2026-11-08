package hubserver

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
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreWorkItemImportSource struct {
	BundlePath     string
	CopyPath       string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Objects        attachment.DetailStore
}

type CoreDependencyImportSource struct {
	BundlePath     string
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Objects        attachment.DetailStore
}

type coreDependencyImportRow struct {
	DependentID         int64
	BlockerID           int64
	DependentProjectID  tracker.ProjectID
	DependentWorkItem   tracker.NativeWorkItemID
	BlockerProjectID    tracker.ProjectID
	BlockerWorkItem     tracker.NativeWorkItemID
	BlockerOrganization sql.NullString
	Provenance          string
	SourceVersion       sql.NullString
	CreatedAt           string
	UpdatedAt           string
}

type coreWorkItemImportRow struct {
	Item                 corestore.WorkItem
	Body                 string
	Contract             sql.NullString
	ReportContext        sql.NullString
	Actor                string
	Provenance           sql.NullString
	Labels               string
	Assignees            string
	Author               string
	Repository           string
	DispatchWaitSince    sql.NullString
	WorkflowState        sql.NullString
	QueueRows            []claimstore.QueueEntry
	Comments             []corestore.Comment
	CommentBodies        map[string]string
	Versions             []eventstore.CollaborationVersion
	Events               []eventstore.Event
	CurrentIssueSnapshot tracker.NativeIssue
}

func ImportCoreWorkItems(ctx context.Context, target *postgres.Repository, source CoreWorkItemImportSource) (int64, error) {
	if source.CopyPath != "" {
		return 0, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.ProjectID == "" || source.Objects == nil {
		return 0, errors.New("invalid core work item import source")
	}
	objects := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := postgres.ValidateNeutralCoreImportTables(bundle, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "change_content_objects", "attachments"); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var after int64
		for {
			rows, err := readCoreWorkItemImportPage(ctx, tx, source, after)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				record, err := prepareCoreWorkItemImport(ctx, tx, objects, source, copyHash, row)
				if err != nil {
					return fmt.Errorf("prepare core work item %d: %w", row.Item.ID, err)
				}
				if err := target.ImportCoreWorkItem(ctx, record); err != nil {
					return fmt.Errorf("import core work item %d: %w", row.Item.ID, err)
				}
				after = row.Item.ID
				imported++
			}
		}
	})
	return imported, err
}

func ImportCoreIssueDependencies(ctx context.Context, target *postgres.Repository, source CoreDependencyImportSource) (int64, error) {
	if source.CopyPath != "" {
		return 0, postgres.ErrNeutralImport
	}
	if target == nil || source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, errors.New("invalid core dependency import source")
	}
	objects := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *postgres.VerifiedNeutralImport) error {
		if err := postgres.ValidateNeutralCoreImportTables(bundle, "issues", "issue_dependencies"); err != nil {
			return err
		}
		tx := newCoreNeutralImport(bundle)
		copyHash := bundle.Identity().SourceSHA256
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var afterDependent, afterBlocker int64
		for {
			rows, err := readCoreDependencyImportPage(ctx, tx, source.OrganizationID, afterDependent, afterBlocker)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				record, err := prepareCoreDependencyImport(ctx, objects, source, copyHash, row)
				if err != nil {
					return fmt.Errorf("prepare core dependency %d/%d: %w", row.DependentID, row.BlockerID, err)
				}
				if err := target.ImportCoreDependency(ctx, record); err != nil {
					return fmt.Errorf("import core dependency %d/%d: %w", row.DependentID, row.BlockerID, err)
				}
				afterDependent, afterBlocker = row.DependentID, row.BlockerID
				imported++
			}
		}
	})
	return imported, err
}

func readCoreDependencyImportPage(ctx context.Context, tx *coreNeutralImport, organization tracker.OrganizationID, afterDependent, afterBlocker int64) ([]coreDependencyImportRow, error) {
	rows, err := tx.rows(ctx, "issue_dependencies", nil, []string{"dependent_issue_id", "blocker_issue_id"}, []string{strconv.FormatInt(afterDependent, 10), strconv.FormatInt(afterBlocker, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreDependencyImportRow, 0, 100)
	for _, sourceRow := range rows {
		var row coreDependencyImportRow
		if err := sourceRow.Decode(map[string]any{"dependent_issue_id": &row.DependentID, "blocker_issue_id": &row.BlockerID, "provenance": &row.Provenance, "source_version": &row.SourceVersion, "created_at": &row.CreatedAt, "updated_at": &row.UpdatedAt}); err != nil {
			return nil, err
		}
		dependent, err := tx.one(ctx, "issues", map[string]string{"organization_id": string(organization), "id": strconv.FormatInt(row.DependentID, 10)}, []string{"id"})
		if err != nil {
			return nil, err
		}
		if err := dependent.Decode(map[string]any{"project_id": &row.DependentProjectID, "native_id": &row.DependentWorkItem}); err != nil {
			return nil, err
		}
		blocker, err := tx.one(ctx, "issues", map[string]string{"id": strconv.FormatInt(row.BlockerID, 10)}, []string{"id"})
		if err != nil {
			return nil, err
		}
		if err := blocker.Decode(map[string]any{"project_id": &row.BlockerProjectID, "native_id": &row.BlockerWorkItem, "organization_id": &row.BlockerOrganization}); err != nil {
			return nil, err
		}
		if row.DependentID < 1 || row.BlockerID < 1 || row.DependentID == row.BlockerID || row.DependentProjectID == "" || row.DependentWorkItem == "" || row.BlockerProjectID == "" || row.BlockerWorkItem == "" || !row.BlockerOrganization.Valid || row.BlockerOrganization.String != string(organization) || row.Provenance == "" {
			return nil, corestore.ErrInvalidRelationship
		}
		result = append(result, row)
	}

	return result, nil
}

func prepareCoreDependencyImport(ctx context.Context, objects *eventstore.Content, source CoreDependencyImportSource, copyHash string, row coreDependencyImportRow) (postgres.CoreDependencyImport, error) {
	created, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
	if err != nil {
		return postgres.CoreDependencyImport{}, errors.New("invalid dependency creation timestamp")
	}
	updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	if err != nil {
		return postgres.CoreDependencyImport{}, errors.New("invalid dependency update timestamp")
	}
	request := struct {
		CopyHash          string                   `json:"copy_hash"`
		DependentID       int64                    `json:"dependent_id"`
		BlockerID         int64                    `json:"blocker_id"`
		DependentProject  tracker.ProjectID        `json:"dependent_project"`
		DependentWorkItem tracker.NativeWorkItemID `json:"dependent_work_item"`
		BlockerProject    tracker.ProjectID        `json:"blocker_project"`
		BlockerWorkItem   tracker.NativeWorkItemID `json:"blocker_work_item"`
		Provenance        string                   `json:"provenance"`
		SourceVersion     *string                  `json:"source_version"`
		CreatedAt         string                   `json:"created_at"`
		UpdatedAt         string                   `json:"updated_at"`
	}{copyHash, row.DependentID, row.BlockerID, row.DependentProjectID, row.DependentWorkItem, row.BlockerProjectID, row.BlockerWorkItem, row.Provenance, nullStringPointer(row.SourceVersion), row.CreatedAt, row.UpdatedAt}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return postgres.CoreDependencyImport{}, err
	}
	digest := sha256.Sum256(requestBytes)
	key := strconv.FormatInt(row.DependentID, 10) + ":" + strconv.FormatInt(row.BlockerID, 10)
	receipt, err := objects.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-dependency", CommandKey: key, RequestHash: hex.EncodeToString(digest[:])}, requestBytes)
	if err != nil {
		return postgres.CoreDependencyImport{}, err
	}
	return postgres.CoreDependencyImport{
		OrganizationID:     source.OrganizationID,
		DependentID:        row.DependentID,
		DependentProjectID: row.DependentProjectID,
		DependentWorkItem:  row.DependentWorkItem,
		BlockerID:          row.BlockerID,
		BlockerProjectID:   row.BlockerProjectID,
		BlockerWorkItem:    row.BlockerWorkItem,
		Provenance:         row.Provenance,
		SourceVersion:      nullStringPointer(row.SourceVersion),
		CreatedAt:          created,
		UpdatedAt:          updated,
		Receipt:            receipt,
	}, nil
}

func readCoreWorkItemImportPage(ctx context.Context, tx *coreNeutralImport, source CoreWorkItemImportSource, after int64) ([]coreWorkItemImportRow, error) {
	rows, err := tx.rows(ctx, "issues", map[string]string{"organization_id": string(source.OrganizationID), "project_id": string(source.ProjectID)}, []string{"id"}, []string{strconv.FormatInt(after, 10)}, 100)
	if err != nil {
		return nil, err
	}
	result := make([]coreWorkItemImportRow, 0, 100)
	for _, sourceRow := range rows {
		var row coreWorkItemImportRow
		var nativeSourceKey, created, updated, nativeCreated, nativeUpdated, activity sql.NullString
		item := &row.Item
		item.OrganizationID = source.OrganizationID
		err := sourceRow.Decode(map[string]any{"id": &item.ID, "native_id": &item.NativeID, "project_id": &item.ProjectID, "number": &item.Number, "revision": &item.Revision, "event_sequence": &item.EventSequence, "native_source_key": &nativeSourceKey, "title": &item.Title, "body": &row.Body, "issue_contract_json": &row.Contract, "report_context_json": &row.ReportContext, "actor_json": &row.Actor, "provenance_json": &row.Provenance, "archived": &item.Archived, "created_at": &created, "updated_at": &updated, "native_created_at": &nativeCreated, "native_updated_at": &nativeUpdated, "last_activity_at": &activity, "dispatch_wait_since": &row.DispatchWaitSince})
		if err != nil {
			return nil, err
		}
		var stateID, repositoryID sql.NullInt64
		var labels, assignees, author sql.NullString
		if err := sourceRow.Decode(map[string]any{"workflow_state_id": &stateID, "repository_id": &repositoryID, "labels_json": &labels, "assignees_json": &assignees, "author_login": &author}); err != nil {
			return nil, err
		}
		row.Labels, row.Assignees, row.Author = "[]", "[]", author.String
		if labels.Valid {
			row.Labels = labels.String
		}
		if assignees.Valid {
			row.Assignees = assignees.String
		}
		row.WorkflowState = sql.NullString{Valid: true}
		if stateID.Valid {
			state, err := tx.one(ctx, "workflow_states", map[string]string{"id": strconv.FormatInt(stateID.Int64, 10)}, []string{"id"})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				if err := state.Decode(map[string]any{"detent_state": &row.WorkflowState}); err != nil {
					return nil, err
				}
			}
		}
		if repositoryID.Valid {
			repository, err := tx.one(ctx, "repositories", map[string]string{"id": strconv.FormatInt(repositoryID.Int64, 10)}, []string{"id"})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				var owner, name string
				if err := repository.Decode(map[string]any{"github_owner": &owner, "github_name": &name}); err != nil {
					return nil, err
				}
				row.Repository = owner + "/" + name
			}
		}
		if item.ProjectID != source.ProjectID || item.ID < 1 || item.NativeID == "" || item.Number < 1 || item.Revision < 1 {
			return nil, eventstore.ErrInvalidRecord
		}
		item.NativeSourceKey = nullStringPointer(nativeSourceKey)
		item.CreatedAt, err = parseCoreImportTime(created)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = parseCoreImportTime(updated)
		if err != nil {
			return nil, err
		}
		item.NativeCreatedAt, err = parseCoreImportTime(nativeCreated)
		if err != nil {
			return nil, err
		}
		item.NativeUpdatedAt, err = parseCoreImportTime(nativeUpdated)
		if err != nil {
			return nil, err
		}
		item.LastActivityAt, err = parseCoreImportTime(activity)
		if err != nil {
			return nil, err
		}
		item.WorkflowStateName = nullStringPointer(row.WorkflowState)
		result = append(result, row)
	}

	return result, nil
}

func prepareCoreWorkItemImport(ctx context.Context, tx *coreNeutralImport, objects *eventstore.Content, source CoreWorkItemImportSource, copyHash string, row coreWorkItemImportRow) (postgres.CoreWorkItemImport, error) {
	item := row.Item
	if err := json.Unmarshal([]byte(row.Actor), &row.CurrentIssueSnapshot.Actor); err != nil {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	if row.Actor == "" {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	if err := json.Unmarshal([]byte(row.Labels), &row.CurrentIssueSnapshot.Labels); err != nil {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	if err := json.Unmarshal([]byte(row.Assignees), &row.CurrentIssueSnapshot.Assignees); err != nil {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	if row.CurrentIssueSnapshot.Actor.Kind == "" || row.CurrentIssueSnapshot.Actor.PrincipalID == "" {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	encodedBody, err := json.Marshal(row.Body)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	bodyRef, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, encodedBody)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	item.BodyObjectKey = new(bodyRef.Key)
	if row.Contract.Valid && row.Contract.String != "" && row.Contract.String != "null" {
		if !json.Valid([]byte(row.Contract.String)) {
			return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
		}
		ref, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, []byte(row.Contract.String))
		if err != nil {
			return postgres.CoreWorkItemImport{}, err
		}
		item.IssueContractObjectKey = new(ref.Key)
	}
	if row.ReportContext.Valid && row.ReportContext.String != "" && row.ReportContext.String != "null" {
		if !json.Valid([]byte(row.ReportContext.String)) {
			return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
		}
		ref, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, []byte(row.ReportContext.String))
		if err != nil {
			return postgres.CoreWorkItemImport{}, err
		}
		item.ReportContextObjectKey = new(ref.Key)
	}
	item.ActorKind = new(row.CurrentIssueSnapshot.Actor.Kind)
	item.ActorPrincipalID = new(row.CurrentIssueSnapshot.Actor.PrincipalID)
	if row.Provenance.Valid && row.Provenance.String != "" && row.Provenance.String != "null" {
		var provenance tracker.Provenance
		if err := json.Unmarshal([]byte(row.Provenance.String), &provenance); err != nil || provenance.Provider == "" || provenance.ExternalID == "" {
			return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
		}
		item.ProvenanceProvider = new(provenance.Provider)
		item.ProvenanceExternalID = new(provenance.ExternalID)
	}
	var labels, assignees []string
	if err := json.Unmarshal([]byte(row.Labels), &labels); err != nil {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	if err := json.Unmarshal([]byte(row.Assignees), &assignees); err != nil {
		return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
	}
	waitValue, hasWait, err := parseOptionalCoreImportTime(row.DispatchWaitSince)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	var wait *time.Time
	if hasWait {
		wait = &waitValue
	}
	record := postgres.CoreWorkItemImport{Item: item, Labels: labels, Facts: claimstore.WorkItemFacts{OrganizationID: source.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, Author: row.Author, Repository: row.Repository, Assignees: assignees, DispatchWaitSince: wait}}
	if err := readCoreImportQueues(ctx, tx, source, item, &record); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if err := readCoreImportComments(ctx, tx, objects, source, item, &row, &record); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if err := readCoreImportVersions(ctx, tx, source, item, &record); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if err := readCoreImportEvents(ctx, tx, objects, source, item, &record); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if len(record.Versions) == 0 {
		return postgres.CoreWorkItemImport{}, errors.New("work item has no complete immutable history")
	}
	var latestRaw []byte
	for _, version := range record.Versions {
		if version.RecordID == string(item.NativeID) && version.Revision == item.Revision {
			latestRaw, err = reconstructCoreNeutralVersion(ctx, tx, source.Objects, string(source.OrganizationID), string(item.ProjectID), string(item.NativeID), string(item.NativeID), item.Revision)
			if err != nil {
				return postgres.CoreWorkItemImport{}, err
			}
		}
	}
	if len(latestRaw) == 0 {
		return postgres.CoreWorkItemImport{}, errors.New("current issue version is missing")
	}
	var current tracker.NativeIssue
	if err := json.Unmarshal(latestRaw, &current); err != nil || current.OrganizationID != source.OrganizationID || current.ProjectID != item.ProjectID || current.WorkItemID != item.NativeID || int64(current.Number) != item.Number || int64(current.Revision) != item.Revision || current.Title != item.Title || current.Body != row.Body || current.State != row.WorkflowState.String || current.Archived != item.Archived || !current.CreatedAt.Equal(item.NativeCreatedAt) || !current.UpdatedAt.Equal(item.NativeUpdatedAt) || current.Actor.Kind != row.CurrentIssueSnapshot.Actor.Kind || current.Actor.PrincipalID != row.CurrentIssueSnapshot.Actor.PrincipalID || !sameCoreImportStrings(current.Labels, row.CurrentIssueSnapshot.Labels) || !sameCoreImportStrings(current.Assignees, row.CurrentIssueSnapshot.Assignees) {
		return postgres.CoreWorkItemImport{}, errors.New("current issue row does not match immutable history")
	}
	item.IssueType, item.Priority = current.Type, current.Priority
	record.Item = item
	var sourceContract *issuecontract.State
	if row.Contract.Valid && row.Contract.String != "" {
		if err := json.Unmarshal([]byte(row.Contract.String), &sourceContract); err != nil {
			return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
		}
	}
	var sourceReportContext *tracker.ReportContext
	if row.ReportContext.Valid && row.ReportContext.String != "" {
		if err := json.Unmarshal([]byte(row.ReportContext.String), &sourceReportContext); err != nil {
			return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
		}
	}
	var sourceProvenance *tracker.Provenance
	if row.Provenance.Valid && row.Provenance.String != "" {
		if err := json.Unmarshal([]byte(row.Provenance.String), &sourceProvenance); err != nil {
			return postgres.CoreWorkItemImport{}, eventstore.ErrInvalidRecord
		}
	}
	if !reflect.DeepEqual(current.IssueContract, sourceContract) || !reflect.DeepEqual(current.ReportContext, sourceReportContext) || !reflect.DeepEqual(current.Provenance, sourceProvenance) {
		return postgres.CoreWorkItemImport{}, errors.New("current issue details do not match immutable history")
	}
	for _, comment := range record.Comments {
		if importedVersionRevision(record.Versions, comment.ID) != comment.Revision {
			return postgres.CoreWorkItemImport{}, errors.New("current comment version is missing")
		}
		raw, err := reconstructCoreNeutralVersion(ctx, tx, source.Objects, string(source.OrganizationID), string(item.ProjectID), string(item.NativeID), comment.ID, comment.Revision)
		if err != nil {
			return postgres.CoreWorkItemImport{}, err
		}
		var currentComment tracker.NativeComment
		if err := json.Unmarshal(raw, &currentComment); err != nil || currentComment.ID != comment.ID || currentComment.OrganizationID != source.OrganizationID || currentComment.ProjectID != item.ProjectID || currentComment.WorkItemID != item.NativeID || int64(currentComment.Revision) != comment.Revision || currentComment.Sequence != comment.Sequence || currentComment.Body != row.CommentBodies[comment.ID] || currentComment.Actor.Kind != comment.ActorKind || currentComment.Actor.PrincipalID != comment.ActorPrincipalID || currentComment.Deleted != comment.Deleted || !currentComment.CreatedAt.Equal(comment.CreatedAt) || !currentComment.UpdatedAt.Equal(comment.UpdatedAt) || !sameCoreImportActor(currentComment.EditedBy, comment.EditedByKind, comment.EditedByPrincipalID) || !sameCoreImportProvenance(currentComment.Provenance, comment.ProvenanceProvider, comment.ProvenanceExternalID) {
			return postgres.CoreWorkItemImport{}, errors.New("current comment row does not match immutable history")
		}
	}
	requestBytes, err := json.Marshal(struct {
		CopyHash string `json:"copy_hash"`
		ID       int64  `json:"id"`
	}{copyHash, item.ID})
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	digest := sha256.Sum256(requestBytes)
	receipt := eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-work-item", CommandKey: strconv.FormatInt(item.ID, 10), RequestHash: hex.EncodeToString(digest[:])}
	record.Receipt, err = objects.PrepareReceipt(ctx, receipt, requestBytes)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	return record, nil
}

func readCoreImportQueues(ctx context.Context, tx *coreNeutralImport, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	return tx.each(ctx, "queue_entries", map[string]string{"issue_id": strconv.FormatInt(item.ID, 10)}, []string{"id"}, func(sourceRow postgres.NeutralImportRow) error {
		var err error

		var queue claimstore.QueueEntry
		var priority sql.NullInt64
		var created, updated string
		if err := sourceRow.Decode(map[string]any{"scope": &queue.Scope, "state": &queue.State, "rank": &queue.Rank, "priority_override": &priority, "created_at": &created, "updated_at": &updated}); err != nil {
			return err
		}
		queue.OrganizationID, queue.ProjectID, queue.WorkItemID = source.OrganizationID, item.ProjectID, item.NativeID
		queue.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return errors.New("invalid queue creation timestamp")
		}
		queue.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return errors.New("invalid queue update timestamp")
		}
		if priority.Valid {
			queue.Priority = &priority.Int64
		}
		record.Queues = append(record.Queues, queue)
		return nil
	})
}

func readCoreImportComments(ctx context.Context, tx *coreNeutralImport, objects *eventstore.Content, source CoreWorkItemImportSource, item corestore.WorkItem, row *coreWorkItemImportRow, record *postgres.CoreWorkItemImport) error {
	row.CommentBodies = make(map[string]string)
	return tx.each(ctx, "native_comments", map[string]string{"organization_id": string(source.OrganizationID), "project_id": string(item.ProjectID), "work_item_id": string(item.NativeID)}, []string{"sequence"}, func(sourceRow postgres.NeutralImportRow) error {
		var err error

		var comment corestore.Comment
		var body, actorJSON string
		var editedJSON, provenanceJSON, sourceKey sql.NullString
		var created, updated string
		if err := sourceRow.Decode(map[string]any{"id": &comment.ID, "revision": &comment.Revision, "sequence": &comment.Sequence, "body": &body, "actor_json": &actorJSON, "edited_by_json": &editedJSON, "provenance_json": &provenanceJSON, "source_key": &sourceKey, "deleted": &comment.Deleted, "created_at": &created, "updated_at": &updated}); err != nil {
			return err
		}
		comment.OrganizationID, comment.ProjectID, comment.WorkItemID = source.OrganizationID, item.ProjectID, item.NativeID
		comment.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return errors.New("invalid comment creation timestamp")
		}
		comment.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return errors.New("invalid comment update timestamp")
		}
		var actor tracker.Actor
		if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil || actor.Kind == "" || actor.PrincipalID == "" {
			return eventstore.ErrInvalidRecord
		}
		comment.ActorKind, comment.ActorPrincipalID = actor.Kind, actor.PrincipalID
		if editedJSON.Valid && editedJSON.String != "" && editedJSON.String != "null" {
			var edited tracker.Actor
			if err := json.Unmarshal([]byte(editedJSON.String), &edited); err != nil || edited.Kind == "" || edited.PrincipalID == "" {
				return eventstore.ErrInvalidRecord
			}
			comment.EditedByKind, comment.EditedByPrincipalID = &edited.Kind, &edited.PrincipalID
		}
		if provenanceJSON.Valid && provenanceJSON.String != "" && provenanceJSON.String != "null" {
			var provenance tracker.Provenance
			if err := json.Unmarshal([]byte(provenanceJSON.String), &provenance); err != nil || provenance.Provider == "" || provenance.ExternalID == "" {
				return eventstore.ErrInvalidRecord
			}
			comment.ProvenanceProvider, comment.ProvenanceExternalID = &provenance.Provider, &provenance.ExternalID
		}
		if sourceKey.Valid {
			comment.SourceKey = &sourceKey.String
		}
		encodedBody, err := json.Marshal(body)
		if err != nil {
			return err
		}
		ref, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, encodedBody)
		if err != nil {
			return err
		}
		comment.BodyObjectKey = ref.Key
		record.Comments = append(record.Comments, comment)
		row.CommentBodies[comment.ID] = body
		return nil
	})
}

func readCoreImportVersions(ctx context.Context, tx *coreNeutralImport, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	content := eventstore.NewContent(source.Objects)
	lastID := ""
	lastRevision := int64(0)
	return tx.each(ctx, "collaboration_versions", map[string]string{"organization_id": string(source.OrganizationID), "project_id": string(item.ProjectID), "work_item_id": string(item.NativeID)}, []string{"record_id", "revision"}, func(sourceRow postgres.NeutralImportRow) error {
		var id string
		var revision int64
		if err := sourceRow.Decode(map[string]any{"record_id": &id, "revision": &revision}); err != nil {
			return err
		}
		if revision < 1 || id == "" || id == lastID && revision <= lastRevision {
			return errors.New("incomplete collaboration version history")
		}
		lastID, lastRevision = id, revision
		raw, err := reconstructCoreNeutralVersion(ctx, tx, source.Objects, string(source.OrganizationID), string(item.ProjectID), string(item.NativeID), id, revision)
		if err != nil {
			return fmt.Errorf("reconstruct collaboration version %s/%d: %w", id, revision, err)
		}
		row, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: source.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, RecordID: id, Revision: revision}, raw)
		if err != nil {
			return err
		}
		record.Versions = append(record.Versions, row)
		return nil
	})
}

func readCoreImportEvents(ctx context.Context, tx *coreNeutralImport, objects *eventstore.Content, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	return tx.each(ctx, "collaboration_events", map[string]string{"organization_id": string(source.OrganizationID), "project_id": string(item.ProjectID), "work_item_id": string(item.NativeID)}, []string{"sequence"}, func(sourceRow postgres.NeutralImportRow) error {
		var err error

		var id, eventType, actorJSON, dataJSON, recorded string
		var sequence, schemaVersion, eventRevision int64
		if err := sourceRow.Decode(map[string]any{"id": &id, "sequence": &sequence, "type": &eventType, "schema_version": &schemaVersion, "actor_json": &actorJSON, "data_json": &dataJSON, "event_revision": &eventRevision, "recorded_at": &recorded}); err != nil {
			return err
		}
		var actor tracker.Actor
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil || actor.Kind == "" || actor.PrincipalID == "" {
			return eventstore.ErrInvalidRecord
		}
		if err := decodeCoreReportData(ctx, tx, source, []byte(dataJSON), &data); err != nil {
			return eventstore.ErrInvalidRecord
		}
		recordedAt, err := time.Parse(time.RFC3339Nano, recorded)
		if err != nil {
			return eventstore.ErrInvalidRecord
		}
		value := tracker.CollaborationEvent{ID: id, OrganizationID: source.OrganizationID, ProjectID: item.ProjectID, AggregateType: "work_item", AggregateID: item.NativeID, AggregateSequence: sequence, Type: eventType, SchemaVersion: int(schemaVersion), RecordedAt: recordedAt, Actor: actor, Data: data}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		event, err := objects.PrepareEvent(ctx, source.OrganizationID, raw)
		if err != nil {
			return err
		}
		event.EventRevision = eventRevision
		record.Events = append(record.Events, event)
		return nil
	})
}

func verifiedCoreImportObject(ctx context.Context, objects attachment.DetailStore, organization tracker.OrganizationID, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := objects.StoreDetail(ctx, string(organization), raw)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(raw)) {
		return attachment.ArchiveReference{}, errors.New("invalid imported object receipt")
	}
	stored, err := objects.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	if !bytes.Equal(stored, raw) {
		return attachment.ArchiveReference{}, errors.New("imported object bytes do not match source")
	}
	return ref, nil
}

func parseCoreImportTime(raw sql.NullString) (time.Time, error) {
	if !raw.Valid || raw.String == "" {
		return time.Time{}, errors.New("missing imported timestamp")
	}
	return time.Parse(time.RFC3339Nano, raw.String)
}

func parseOptionalCoreImportTime(raw sql.NullString) (time.Time, bool, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return time.Time{}, false, nil
	}
	value, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return time.Time{}, false, errors.New("invalid optional imported timestamp")
	}
	return value, true, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func sameCoreImportStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameCoreImportActor(actor *tracker.Actor, kind, principal *string) bool {
	if actor == nil {
		return kind == nil && principal == nil
	}
	return kind != nil && principal != nil && actor.Kind == *kind && actor.PrincipalID == *principal
}

func sameCoreImportProvenance(provenance *tracker.Provenance, provider, externalID *string) bool {
	if provenance == nil {
		return provider == nil && externalID == nil
	}
	return provider != nil && externalID != nil && provenance.Provider == *provider && provenance.ExternalID == *externalID
}

func importedVersionRevision(versions []eventstore.CollaborationVersion, recordID string) int64 {
	var revision int64
	for _, version := range versions {
		if version.RecordID == recordID && version.Revision > revision {
			revision = version.Revision
		}
	}
	return revision
}
