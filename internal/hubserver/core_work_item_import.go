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
	CopyPath       string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Objects        attachment.DetailStore
}

type CoreDependencyImportSource struct {
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
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.ProjectID == "" || source.Objects == nil {
		return 0, errors.New("invalid core work item import source")
	}
	objects := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := target.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
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
	if target == nil || source.CopyPath == "" || source.OrganizationID == "" || source.Objects == nil {
		return 0, errors.New("invalid core dependency import source")
	}
	objects := eventstore.NewContent(source.Objects)
	var imported int64
	err := postgres.ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := target.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
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

func readCoreDependencyImportPage(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, afterDependent, afterBlocker int64) ([]coreDependencyImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT d.dependent_issue_id,d.blocker_issue_id,dependent.project_id,dependent.native_id,blocker.project_id,blocker.native_id,blocker.organization_id,d.provenance,d.source_version,d.created_at,d.updated_at
FROM issue_dependencies d
JOIN issues dependent ON dependent.id=d.dependent_issue_id
LEFT JOIN issues blocker ON blocker.id=d.blocker_issue_id
WHERE dependent.organization_id=?
AND (d.dependent_issue_id>? OR (d.dependent_issue_id=? AND d.blocker_issue_id>?))
ORDER BY d.dependent_issue_id,d.blocker_issue_id LIMIT 100`, organization, afterDependent, afterDependent, afterBlocker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreDependencyImportRow, 0, 100)
	for rows.Next() {
		var row coreDependencyImportRow
		if err := rows.Scan(&row.DependentID, &row.BlockerID, &row.DependentProjectID, &row.DependentWorkItem, &row.BlockerProjectID, &row.BlockerWorkItem, &row.BlockerOrganization, &row.Provenance, &row.SourceVersion, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		if row.DependentID < 1 || row.BlockerID < 1 || row.DependentID == row.BlockerID || row.DependentProjectID == "" || row.DependentWorkItem == "" || row.BlockerProjectID == "" || row.BlockerWorkItem == "" || !row.BlockerOrganization.Valid || row.BlockerOrganization.String != string(organization) || row.Provenance == "" {
			return nil, corestore.ErrInvalidRelationship
		}
		result = append(result, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
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

func readCoreWorkItemImportPage(ctx context.Context, tx *sql.Tx, source CoreWorkItemImportSource, after int64) ([]coreWorkItemImportRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.id,i.native_id,i.project_id,i.number,i.revision,i.event_sequence,i.native_source_key,
i.title,i.body,i.issue_contract_json,i.report_context_json,i.actor_json,i.provenance_json,i.archived,
i.created_at,i.updated_at,i.native_created_at,i.native_updated_at,i.last_activity_at,
COALESCE(ws.detent_state,''),COALESCE(i.labels_json,'[]'),COALESCE(i.assignees_json,'[]'),COALESCE(i.author_login,''),
COALESCE(r.github_owner || '/' || r.github_name,''),i.dispatch_wait_since
FROM issues i
LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
LEFT JOIN repositories r ON r.id=i.repository_id
WHERE i.organization_id=? AND i.project_id=? AND i.id>? ORDER BY i.id LIMIT 100`, source.OrganizationID, source.ProjectID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]coreWorkItemImportRow, 0, 100)
	for rows.Next() {
		var row coreWorkItemImportRow
		var nativeSourceKey, created, updated, nativeCreated, nativeUpdated, activity sql.NullString
		item := &row.Item
		item.OrganizationID = source.OrganizationID
		err := rows.Scan(&item.ID, &item.NativeID, &item.ProjectID, &item.Number, &item.Revision, &item.EventSequence, &nativeSourceKey,
			&item.Title, &row.Body, &row.Contract, &row.ReportContext, &row.Actor, &row.Provenance, &item.Archived,
			&created, &updated, &nativeCreated, &nativeUpdated, &activity,
			&row.WorkflowState, &row.Labels, &row.Assignees, &row.Author, &row.Repository, &row.DispatchWaitSince)
		if err != nil {
			return nil, err
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
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return result, nil
}

func prepareCoreWorkItemImport(ctx context.Context, tx *sql.Tx, objects *eventstore.Content, source CoreWorkItemImportSource, copyHash string, row coreWorkItemImportRow) (postgres.CoreWorkItemImport, error) {
	var objectVersions bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('collaboration_versions') WHERE name='payload_object_key')").Scan(&objectVersions); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	ctx = context.WithValue(ctx, nativeVersionStorageKey{}, objectVersions)
	ctx = context.WithValue(ctx, changeContentKey{}, &changeContent{query: tx, store: source.Objects})
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
			latestRaw, err = reconstructNativeVersion(ctx, tx, string(source.OrganizationID), string(item.ProjectID), string(item.NativeID), string(item.NativeID), item.Revision)
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
		raw, err := reconstructNativeVersion(ctx, tx, string(source.OrganizationID), string(item.ProjectID), string(item.NativeID), comment.ID, comment.Revision)
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

func readCoreImportQueues(ctx context.Context, tx *sql.Tx, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	rows, err := tx.QueryContext(ctx, `SELECT q.scope,q.state,q.rank,q.priority_override,q.created_at,q.updated_at
FROM queue_entries q WHERE q.issue_id=$1 ORDER BY q.id`, item.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var queue claimstore.QueueEntry
		var priority sql.NullInt64
		var created, updated string
		if err := rows.Scan(&queue.Scope, &queue.State, &queue.Rank, &priority, &created, &updated); err != nil {
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
	}
	return errors.Join(rows.Err(), rows.Close())
}

func readCoreImportComments(ctx context.Context, tx *sql.Tx, objects *eventstore.Content, source CoreWorkItemImportSource, item corestore.WorkItem, row *coreWorkItemImportRow, record *postgres.CoreWorkItemImport) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,sequence,body,actor_json,edited_by_json,provenance_json,source_key,deleted,created_at,updated_at
FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY sequence`, source.OrganizationID, item.ProjectID, item.NativeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	row.CommentBodies = make(map[string]string)
	for rows.Next() {
		var comment corestore.Comment
		var body, actorJSON string
		var editedJSON, provenanceJSON, sourceKey sql.NullString
		var created, updated string
		if err := rows.Scan(&comment.ID, &comment.Revision, &comment.Sequence, &body, &actorJSON, &editedJSON, &provenanceJSON, &sourceKey, &comment.Deleted, &created, &updated); err != nil {
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
	}
	return errors.Join(rows.Err(), rows.Close())
}

func readCoreImportVersions(ctx context.Context, tx *sql.Tx, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	rows, err := tx.QueryContext(ctx, `SELECT record_id,revision FROM collaboration_versions
WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY record_id,revision`, source.OrganizationID, item.ProjectID, item.NativeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	type versionKey struct {
		recordID string
		revision int64
	}
	keys := make([]versionKey, 0, 16)
	for rows.Next() {
		var key versionKey
		if err := rows.Scan(&key.recordID, &key.revision); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	content := eventstore.NewContent(source.Objects)
	lastID := ""
	lastRevision := int64(0)
	for _, key := range keys {
		if key.revision < 1 || key.recordID == "" || key.recordID == lastID && key.revision <= lastRevision {
			return errors.New("incomplete collaboration version history")
		}
		lastID, lastRevision = key.recordID, key.revision
		raw, err := reconstructNativeVersion(ctx, tx, string(source.OrganizationID), string(item.ProjectID), string(item.NativeID), key.recordID, key.revision)
		if err != nil {
			return fmt.Errorf("reconstruct collaboration version %s/%d: %w", key.recordID, key.revision, err)
		}
		row, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: source.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, RecordID: key.recordID, Revision: key.revision}, raw)
		if err != nil {
			return err
		}
		record.Versions = append(record.Versions, row)
	}
	return nil
}

func readCoreImportEvents(ctx context.Context, tx *sql.Tx, objects *eventstore.Content, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,sequence,type,schema_version,actor_json,data_json,event_revision,recorded_at
FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY sequence`, source.OrganizationID, item.ProjectID, item.NativeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, eventType, actorJSON, dataJSON, recorded string
		var sequence, schemaVersion, eventRevision int64
		if err := rows.Scan(&id, &sequence, &eventType, &schemaVersion, &actorJSON, &dataJSON, &eventRevision, &recorded); err != nil {
			return err
		}
		var actor tracker.Actor
		var data tracker.CollaborationData
		if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil || actor.Kind == "" || actor.PrincipalID == "" {
			return eventstore.ErrInvalidRecord
		}
		if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
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
	}
	return errors.Join(rows.Err(), rows.Close())
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
