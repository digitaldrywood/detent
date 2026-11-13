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
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CoreWorkItemImportSource struct {
	BundlePath       string
	CopyPath         string
	OrganizationID   tracker.OrganizationID
	ProjectID        tracker.ProjectID
	Objects          attachment.DetailStore
	IdentityMappings []CoreWorkItemIdentityMapping
	Logger           *slog.Logger
	activity         map[string]coreHistoricalActivity
	activityDigest   string
}

type coreHistoricalSourceReference struct {
	Table      string
	CommandKey string
}

func prepareCoreHistoricalSourceRow(ctx context.Context, content *eventstore.Content, organization tracker.OrganizationID, sourceSHA string, id int64, table string, row postgres.NeutralImportRow) (eventstore.Receipt, error) {
	raw, err := json.Marshal(row)
	if err != nil {
		return eventstore.Receipt{}, err
	}
	return content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization, ActorID: "production-import", Operation: "core-work-item-source-row", CommandKey: strconv.FormatInt(id, 10) + "/" + table, RequestHash: artifact.Digest(append([]byte(sourceSHA+"\n"), raw...))}, raw)
}

type coreHistoricalActivityRow struct {
	Table        string
	Row          postgres.NeutralImportRow
	RecordSHA256 string
	Timestamps   []string
}
type coreHistoricalActivity struct {
	At   time.Time
	Rows []coreHistoricalActivityRow
}

type CoreWorkItemIdentityMapping struct {
	SourceSHA256           string
	OrganizationID         tracker.OrganizationID
	SourceIssueID          int64
	SourceRepositoryID     int64
	SourceRepositoryNodeID string
	SourceGitHubNodeID     string
	TargetProjectID        tracker.ProjectID
	TargetWorkItemID       tracker.NativeWorkItemID
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
	Item                   corestore.WorkItem
	Body                   string
	Contract               sql.NullString
	ReportContext          sql.NullString
	Actor                  string
	Provenance             sql.NullString
	Labels                 string
	Assignees              string
	Author                 string
	Repository             string
	SourceRepositoryID     sql.NullInt64
	SourceRepositoryNodeID string
	SourceGitHubNodeID     sql.NullString
	SourceProvider         sql.NullString
	SourceExternalID       sql.NullString
	SourceGitHubNumber     sql.NullInt64
	SourceGitHubURL        sql.NullString
	SourceRow              postgres.NeutralImportRow
	SourceRepositoryRow    *postgres.NeutralImportRow
	SourceActivityRows     []coreHistoricalActivityRow
	DispatchWaitSince      sql.NullString
	WorkflowState          sql.NullString
	TerminalEnteredAt      sql.NullString
	WorkflowTerminal       sql.NullBool
	QueueRows              []claimstore.QueueEntry
	Comments               []corestore.Comment
	CommentBodies          map[string]string
	Versions               []eventstore.CollaborationVersion
	Events                 []eventstore.Event
	CurrentIssueSnapshot   tracker.NativeIssue
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
		if _, native := bundle.Schema()["issues"]["native_id"]; !native {
			if err := postgres.ValidateNeutralHistoricalWorkItemTables(bundle); err != nil {
				return err
			}
			var err error
			imported, err = importCoreHistoricalWorkItems(ctx, target, bundle, source)
			return err
		}
		if err := postgres.ValidateNeutralCoreImportTables(bundle, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "change_content_objects", "attachments"); err != nil {
			return err
		}
		if _, exists := bundle.Schema()["linked_issue_sources"]; exists {
			if err := postgres.ValidateNeutralCoreImportTables(bundle, "linked_issue_sources"); err != nil {
				return err
			}
		}
		tx := newCoreNeutralImport(bundle)
		if source.activity == nil {
			var err error
			source.activity, err = readCoreHistoricalActivity(ctx, bundle, source.OrganizationID)
			if err != nil {
				return err
			}
			source.activityDigest = bundle.Digest()
		} else if source.activityDigest != bundle.Digest() {
			return postgres.ErrNeutralImport
		}
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
		row.SourceRow = sourceRow
		item.OrganizationID = source.OrganizationID
		err := sourceRow.Decode(map[string]any{"id": &item.ID, "native_id": &item.NativeID, "project_id": &item.ProjectID, "number": &item.Number, "revision": &item.Revision, "event_sequence": &item.EventSequence, "native_source_key": &nativeSourceKey, "title": &item.Title, "body": &row.Body, "issue_contract_json": &row.Contract, "report_context_json": &row.ReportContext, "actor_json": &row.Actor, "provenance_json": &row.Provenance, "github_node_id": &row.SourceGitHubNodeID, "github_number": &row.SourceGitHubNumber, "url": &row.SourceGitHubURL, "archived": &item.Archived, "created_at": &created, "updated_at": &updated, "native_created_at": &nativeCreated, "native_updated_at": &nativeUpdated, "last_activity_at": &activity, "dispatch_wait_since": &row.DispatchWaitSince, "terminal_entered_at": &row.TerminalEnteredAt})
		if err != nil {
			return nil, err
		}
		for name, destination := range map[string]any{"provenance_provider": &row.SourceProvider, "provenance_external_id": &row.SourceExternalID} {
			if _, present := sourceRow.Values[name]; present {
				if err := sourceRow.Decode(map[string]any{name: destination}); err != nil {
					return nil, err
				}
			}
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
				if err := state.Decode(map[string]any{"detent_state": &row.WorkflowState, "terminal": &row.WorkflowTerminal}); err != nil {
					return nil, err
				}
			}
		}
		if repositoryID.Valid {
			row.SourceRepositoryID = repositoryID
			repository, err := tx.one(ctx, "repositories", map[string]string{"id": strconv.FormatInt(repositoryID.Int64, 10)}, []string{"id"})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				row.SourceRepositoryRow = &repository
				var owner, name string
				if err := repository.Decode(map[string]any{"github_owner": &owner, "github_name": &name, "github_node_id": &row.SourceRepositoryNodeID}); err != nil {
					return nil, err
				}
				row.Repository = owner + "/" + name
			}
		}
		if item.ProjectID != source.ProjectID || item.ID < 1 || item.NativeID == "" || item.Number < 1 || item.Revision < 1 {
			return nil, eventstore.ErrInvalidRecord
		}
		item.NativeSourceKey = nullStringPointer(nativeSourceKey)
		for _, mapping := range source.IdentityMappings {
			if mapping.SourceIssueID != item.ID {
				continue
			}
			if mapping.SourceSHA256 != tx.bundle.Identity().SourceSHA256 || mapping.OrganizationID != source.OrganizationID || mapping.SourceRepositoryID != repositoryID.Int64 || mapping.SourceRepositoryNodeID != row.SourceRepositoryNodeID || mapping.SourceGitHubNodeID != row.SourceGitHubNodeID.String || mapping.TargetProjectID != item.ProjectID || mapping.TargetWorkItemID != item.NativeID {
				return nil, corestore.ErrRevisionConflict
			}
		}
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
		if historical, ok := source.activity[string(item.NativeID)]; ok {
			if historical.At.After(item.LastActivityAt) {
				item.LastActivityAt = historical.At
			}
			row.SourceActivityRows = historical.Rows
		}

		item.WorkflowStateName = nullStringPointer(row.WorkflowState)
		entered, known, err := parseOptionalCoreImportTime(row.TerminalEnteredAt)
		if err != nil {
			return nil, err
		}
		if known {
			if !row.WorkflowTerminal.Valid || !row.WorkflowTerminal.Bool || entered.Before(item.NativeCreatedAt) || entered.After(item.NativeUpdatedAt) {
				return nil, eventstore.ErrInvalidRecord
			}
			item.TerminalEnteredAt = &entered
		}
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
	if row.SourceGitHubNodeID.Valid && row.SourceGitHubNodeID.String != "" && item.ProvenanceProvider == nil {
		item.ProvenanceProvider = new("github")
		item.ProvenanceExternalID = new(row.SourceGitHubNodeID.String)
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
	pendingVersions := make(map[eventstore.CollaborationVersionKey][]byte)
	pendingEvents := make(map[string][]byte)
	reference := coreGitHubReferenceEvidence{}
	provider, node := stringValue(item.ProvenanceProvider), stringValue(item.ProvenanceExternalID)
	if row.SourceProvider.Valid {
		provider = row.SourceProvider.String
	}
	if row.SourceExternalID.Valid && row.SourceExternalID.String != "" {
		node = row.SourceExternalID.String
	} else if row.SourceGitHubNodeID.String != "" {
		node = row.SourceGitHubNodeID.String
	}
	knownNumber := row.SourceGitHubNumber.Valid && row.SourceGitHubNumber.Int64 > 0 && row.Repository != ""
	if node == "" && knownNumber {
		node = "https://github.com/" + row.Repository + "/issues/" + strconv.FormatInt(row.SourceGitHubNumber.Int64, 10)
	}
	reference.nodeID = node
	reference.eligible = provider == "github" || row.SourceGitHubNodeID.String != "" || knownNumber
	if err := readCoreImportVersions(ctx, tx, source, item, &record, pendingVersions, &reference); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if err := readCoreImportLinkedSource(ctx, tx, objects, source, item, &record); err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if err := readCoreImportEvents(ctx, tx, objects, source, item, &record, pendingEvents); err != nil {
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
	item.ArchiveRestoredAt, item.ArchiveRestoreKnown = record.Item.ArchiveRestoredAt, record.Item.ArchiveRestoreKnown
	record.Item = item
	document, err := searchstore.NewContent(source.Objects).Prepare(ctx, searchstore.Document{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: "issue:" + string(item.NativeID), SourceID: string(item.NativeID), Kind: "issue", Scope: "issue", Revision: item.Revision, UpdatedAt: current.UpdatedAt}, searchstore.Text{Title: current.Title, Body: current.Body, Labels: current.Labels})
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	record.Search = &document
	var sourceContract *issuecontract.State
	if row.Contract.Valid && row.Contract.String != "" && (strings.TrimSpace(row.Contract.String) != "{}" || current.IssueContract != nil) {
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
	resolved, err := resolveCoreGitHubReference(ctx, tx, source, row, current, &reference)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	if resolved != nil {
		references := []tracker.ExternalReference{*resolved}
		for _, version := range record.Versions {
			if version.RecordID != string(item.NativeID) || version.Revision != item.Revision {
				continue
			}
			key := eventstore.CollaborationVersionKey{RecordID: version.RecordID, Revision: version.Revision}
			raw, found := pendingVersions[key]
			if !found {
				raw, err = objects.CollaborationVersion(ctx, version)
				if err != nil {
					return postgres.CoreWorkItemImport{}, err
				}
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return postgres.CoreWorkItemImport{}, err
			}
			var existing []tracker.ExternalReference
			if value, found := fields["external_references"]; found {
				if err := json.Unmarshal(value, &existing); err != nil {
					return postgres.CoreWorkItemImport{}, err
				}
			}
			for _, reference := range references {
				existing = slices.DeleteFunc(existing, func(current tracker.ExternalReference) bool {
					return current.Provider == reference.Provider && current.Kind == reference.Kind && current.ID == reference.ID
				})
				existing = append(existing, reference)
			}
			fields["external_references"], err = json.Marshal(existing)
			if err != nil {
				return postgres.CoreWorkItemImport{}, err
			}
			pendingVersions[key], err = json.Marshal(fields)
			if err != nil {
				return postgres.CoreWorkItemImport{}, err
			}
		}
	}
	if err := normalizeCoreImportedHealthActors(ctx, objects, &record, pendingVersions, pendingEvents); err != nil {
		return postgres.CoreWorkItemImport{}, err
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
	sourceRows := []coreHistoricalSourceReference{}
	sourceRowReceipt, err := prepareCoreHistoricalSourceRow(ctx, objects, source.OrganizationID, copyHash, item.ID, "issues", row.SourceRow)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	record.SourceReceipts = append(record.SourceReceipts, sourceRowReceipt)
	sourceRows = append(sourceRows, coreHistoricalSourceReference{Table: "issues", CommandKey: sourceRowReceipt.CommandKey})
	if row.SourceRepositoryRow != nil {
		repositoryReceipt, err := prepareCoreHistoricalSourceRow(ctx, objects, source.OrganizationID, copyHash, item.ID, "repositories", *row.SourceRepositoryRow)
		if err != nil {
			return postgres.CoreWorkItemImport{}, err
		}
		record.SourceReceipts = append(record.SourceReceipts, repositoryReceipt)
		sourceRows = append(sourceRows, coreHistoricalSourceReference{Table: "repositories", CommandKey: repositoryReceipt.CommandKey})
	}
	sourceBytes, err := json.Marshal(struct {
		Mapping      CoreWorkItemIdentityMapping
		SourceRows   []coreHistoricalSourceReference
		ActivityRows []coreHistoricalActivityRow
	}{CoreWorkItemIdentityMapping{SourceSHA256: copyHash, OrganizationID: source.OrganizationID, SourceIssueID: item.ID, SourceRepositoryID: row.SourceRepositoryID.Int64, SourceRepositoryNodeID: row.SourceRepositoryNodeID, SourceGitHubNodeID: row.SourceGitHubNodeID.String, TargetProjectID: item.ProjectID, TargetWorkItemID: item.NativeID}, sourceRows, row.SourceActivityRows})
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	sourceReceipt, err := objects.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-work-item-source", CommandKey: strconv.FormatInt(item.ID, 10), RequestHash: artifact.Digest(sourceBytes)}, sourceBytes)
	if err != nil {
		return postgres.CoreWorkItemImport{}, err
	}
	record.SourceReceipt = &sourceReceipt
	for _, version := range record.Versions {
		if version.RecordID == string(item.NativeID) && version.Revision == item.Revision {
			record.DoctorIdentities = &postgres.DoctorProposalIdentityBinding{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, Revision: item.Revision, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(current.Body)}
		}
	}
	return record, nil
}

func normalizeCoreImportedHealthActors(ctx context.Context, objects *eventstore.Content, record *postgres.CoreWorkItemImport, pendingVersions map[eventstore.CollaborationVersionKey][]byte, pendingEvents map[string][]byte) error {
	if record.Item.ActorKind != nil && record.Item.ActorPrincipalID != nil && *record.Item.ActorKind == "system" && *record.Item.ActorPrincipalID == "health_detector" {
		record.Item.ActorKind = new("integration")
	}
	for i := range record.Comments {
		comment := &record.Comments[i]
		if comment.ActorKind == "system" && comment.ActorPrincipalID == "health_detector" {
			comment.ActorKind = "integration"
		}
		if comment.EditedByKind != nil && comment.EditedByPrincipalID != nil && *comment.EditedByKind == "system" && *comment.EditedByPrincipalID == "health_detector" {
			comment.EditedByKind = new("integration")
		}
	}
	for i := range record.Events {
		row := &record.Events[i]
		raw, found := pendingEvents[row.ID]
		if !found {
			continue
		}
		ref, err := objects.PreparePayload(ctx, row.OrganizationID, raw)
		if err != nil {
			return err
		}
		if row.ActorKind == "system" && row.ActorPrincipalID == "health_detector" {
			row.ActorKind = "integration"
		}
		row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	}
	for i := range record.Versions {
		row := &record.Versions[i]
		raw, found := pendingVersions[eventstore.CollaborationVersionKey{RecordID: row.RecordID, Revision: row.Revision}]
		if !found {
			continue
		}
		prepared, err := objects.PrepareCollaborationVersion(ctx, *row, raw)
		if err != nil {
			return err
		}
		*row = prepared
	}
	return nil
}

func normalizeCoreImportedActorJSON(raw []byte, keys ...string) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false, eventstore.ErrInvalidRecord
	}
	changed := false
	for _, key := range keys {
		value, found := fields[key]
		if !found || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		var actor tracker.Actor
		if err := json.Unmarshal(value, &actor); err != nil {
			return nil, false, eventstore.ErrInvalidRecord
		}
		if actor.Kind != "system" || actor.PrincipalID != "health_detector" {
			continue
		}
		var attributes map[string]json.RawMessage
		if err := json.Unmarshal(value, &attributes); err != nil {
			return nil, false, eventstore.ErrInvalidRecord
		}
		attributes["kind"] = json.RawMessage(`"integration"`)
		encoded, err := json.Marshal(attributes)
		if err != nil {
			return nil, false, err
		}
		fields[key] = encoded
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	normalized, err := json.Marshal(fields)
	return normalized, true, err
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

type coreGitHubReferenceEvidence struct {
	nodeID, url        string
	eligible, conflict bool
}

func (e *coreGitHubReferenceEvidence) add(raw string) {
	canonical, _, _, err := tracker.ParseGitHubIssueURL(raw)
	if err != nil {
		return
	}
	if e.url != "" && e.url != canonical {
		e.conflict = true
	}
	if e.url == "" {
		e.url = canonical
	}
}

func resolveCoreGitHubReference(ctx context.Context, tx *coreNeutralImport, source CoreWorkItemImportSource, row coreWorkItemImportRow, current tracker.NativeIssue, evidence *coreGitHubReferenceEvidence) (*tracker.ExternalReference, error) {
	if !evidence.eligible || evidence.nodeID == "" {
		return nil, nil
	}
	complete := ""
	if row.SourceGitHubNumber.Valid && row.SourceGitHubNumber.Int64 > 0 && row.Repository != "" {
		complete = "https://github.com/" + row.Repository + "/issues/" + strconv.FormatInt(row.SourceGitHubNumber.Int64, 10)
	} else if canonical, _, _, err := tracker.ParseGitHubIssueURL(row.SourceGitHubURL.String); err == nil {
		complete = canonical
	}
	if complete == "" {
		for _, ref := range current.ExternalReferences {
			if ref.Provider == "github" && ref.Kind == "issue" && ref.ID == evidence.nodeID {
				if canonical, repository, number, err := tracker.ParseGitHubIssueURL(ref.URL); err == nil && ref.Repository == repository && ref.Number == number {
					complete = canonical
					break
				}
			}
		}
	}
	if complete != "" {
		ref := tracker.GitHubIssueSourceReference(evidence.nodeID, complete)
		return &ref, nil
	}
	evidence.add(githubMigrationSourceURL(row.Body))
	_, imports := tx.schema["github_imports"]
	_, records := tx.schema["github_import_records"]
	if imports != records {
		return nil, postgres.ErrNeutralImport
	}
	if imports {
		filter := map[string]string{"project_id": string(row.Item.ProjectID), "work_item_id": string(row.Item.NativeID)}
		if _, scoped := tx.schema["github_imports"]["organization_id"]; scoped {
			filter["organization_id"] = string(source.OrganizationID)
		}
		err := tx.each(ctx, "github_imports", filter, []string{"id"}, func(importRow postgres.NeutralImportRow) error {
			var id string
			if err := importRow.Decode(map[string]any{"id": &id}); err != nil || id == "" {
				return postgres.ErrNeutralImport
			}
			fields := map[string]string{"import_id": id, "kind": "issue"}
			if _, scoped := tx.schema["github_import_records"]["organization_id"]; scoped {
				fields["organization_id"] = string(source.OrganizationID)
			}
			return tx.each(ctx, "github_import_records", fields, []string{"sequence"}, func(recordRow postgres.NeutralImportRow) error {
				var raw string
				var external sql.NullString
				if err := recordRow.Decode(map[string]any{"record_json": &raw}); err != nil {
					return err
				}
				if _, present := recordRow.Values["provenance_external_id"]; present {
					if err := recordRow.Decode(map[string]any{"provenance_external_id": &external}); err != nil {
						return err
					}
				}
				var record struct {
					Body       string             `json:"body"`
					Provenance tracker.Provenance `json:"provenance"`
					Data       struct {
						NodeID  string `json:"node_id"`
						URL     string `json:"url"`
						HTMLURL string `json:"html_url"`
					} `json:"data"`
				}
				if err := json.Unmarshal([]byte(raw), &record); err != nil {
					return err
				}
				if external.Valid && external.String != "" && external.String != evidence.nodeID || record.Provenance.ExternalID != "" && record.Provenance.ExternalID != evidence.nodeID || record.Data.NodeID != "" && record.Data.NodeID != evidence.nodeID {
					return nil
				}
				evidence.add(record.Data.URL)
				evidence.add(record.Data.HTMLURL)
				evidence.add(githubMigrationSourceURL(record.Body))
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
	}
	ref := tracker.GitHubIssueSourceReference(evidence.nodeID, evidence.url)
	if row.SourceGitHubNumber.Valid && row.SourceGitHubNumber.Int64 > 0 && int64(ref.Number) != row.SourceGitHubNumber.Int64 || row.Repository != "" && !strings.EqualFold(row.Repository, ref.Repository) {
		evidence.conflict = true
	}
	if ref.URL == "" || evidence.conflict {
		if source.Logger != nil {
			source.Logger.WarnContext(ctx, "GitHub issue reference remains unresolved", "project_id", row.Item.ProjectID, "work_item_id", row.Item.NativeID, "node_id", evidence.nodeID, "conflicting_sources", evidence.conflict)
		}
		return nil, nil
	}
	return &ref, nil
}

func readCoreImportVersions(ctx context.Context, tx *coreNeutralImport, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport, pending map[eventstore.CollaborationVersionKey][]byte, references *coreGitHubReferenceEvidence) error {
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
		if references.eligible && id == string(item.NativeID) {
			var issue tracker.NativeIssue
			if err := json.Unmarshal(raw, &issue); err != nil {
				return err
			}
			if issue.Provenance == nil || issue.Provenance.ExternalID == "" || issue.Provenance.ExternalID == references.nodeID {
				references.add(githubMigrationSourceURL(issue.Body))
			}
		}

		normalized, changed, err := normalizeCoreImportedActorJSON(raw, "actor", "edited_by")
		if err != nil {
			return err
		}
		if changed {
			pending[eventstore.CollaborationVersionKey{RecordID: id, Revision: revision}] = normalized
		}
		row, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: source.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, RecordID: id, Revision: revision}, raw)
		if err != nil {
			return err
		}
		record.Versions = append(record.Versions, row)
		return nil
	})
}

func readCoreImportLinkedSource(ctx context.Context, tx *coreNeutralImport, objects *eventstore.Content, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport) error {
	if _, exists := tx.schema["linked_issue_sources"]; !exists {
		return nil
	}
	row, err := tx.one(ctx, "linked_issue_sources", map[string]string{"work_item_id": string(item.NativeID)}, []string{"work_item_id"})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	linked := eventstore.LinkedSource{OrganizationID: source.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID}
	var id string
	var snapshotJSON sql.NullString
	if err := row.Decode(map[string]any{"work_item_id": &id, "source_url": &linked.URL, "title_supplied": &linked.TitleSupplied, "body_supplied": &linked.BodySupplied, "snapshot_json": &snapshotJSON}); err != nil {
		return err
	}
	if id != string(item.NativeID) {
		return eventstore.ErrInvalidRecord
	}
	if _, _, _, err := tracker.ParseGitHubIssueURL(linked.URL); err != nil {
		return eventstore.ErrInvalidRecord
	}
	if snapshotJSON.Valid {
		raw, err := resolveCoreNeutralJSON(ctx, tx, source.OrganizationID, source.Objects, []byte(snapshotJSON.String))
		if err != nil {
			return err
		}
		var snapshot tracker.GitHubIssueSnapshot
		if json.Unmarshal(raw, &snapshot) != nil || snapshot.URL != linked.URL {
			return eventstore.ErrInvalidRecord
		}
		ref, err := objects.PrepareLinkedIssueSnapshot(ctx, source.OrganizationID, snapshot)
		if err != nil {
			return err
		}
		linked.SnapshotObjectKey, linked.SnapshotSHA256, linked.SnapshotBytes, linked.SnapshotContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	}
	record.LinkedSource = &linked
	return nil
}

func readCoreImportEvents(ctx context.Context, tx *coreNeutralImport, objects *eventstore.Content, source CoreWorkItemImportSource, item corestore.WorkItem, record *postgres.CoreWorkItemImport, pending map[string][]byte) error {
	return tx.each(ctx, "collaboration_events", map[string]string{"organization_id": string(source.OrganizationID), "project_id": string(item.ProjectID), "work_item_id": string(item.NativeID)}, []string{"sequence"}, func(sourceRow postgres.NeutralImportRow) error {
		var err error

		var id, eventType, actorJSON, dataJSON, recorded string
		var sequence, schemaVersion int64
		var eventRevision *int64
		var archiveOperation sql.NullString
		if err := sourceRow.Decode(map[string]any{"id": &id, "sequence": &sequence, "type": &eventType, "schema_version": &schemaVersion, "actor_json": &actorJSON, "data_json": &dataJSON, "event_revision": &eventRevision, "recorded_at": &recorded, "archive_operation": &archiveOperation}); err != nil {
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
		if eventRevision != nil {
			data.Revision = tracker.Revision(*eventRevision)
		}
		if err := normalizeCoreImportPageFacts(sourceRow, &data); err != nil {
			return err
		}
		if record.LinkedSource != nil && eventType == "issue.edited" {
			record.LinkedSource.TitleOwned = record.LinkedSource.TitleOwned || slices.Contains(data.Fields, "title")
			record.LinkedSource.BodyOwned = record.LinkedSource.BodyOwned || slices.Contains(data.Fields, "body")
		}
		recordedAt, err := time.Parse(time.RFC3339Nano, recorded)
		if err != nil {
			return eventstore.ErrInvalidRecord
		}
		if archiveOperation.Valid && archiveOperation.String != "" && data.Operation != "" && archiveOperation.String != data.Operation {
			return eventstore.ErrInvalidRecord
		}
		restore := data.Operation == "restore" || archiveOperation.Valid && archiveOperation.String == "restore"
		if eventType == "issue.edited" && restore {
			if recordedAt.Before(item.NativeCreatedAt) || recordedAt.After(item.NativeUpdatedAt) {
				return eventstore.ErrInvalidRecord
			}
			if record.Item.ArchiveRestoredAt == nil || record.Item.ArchiveRestoredAt.Before(recordedAt) {
				record.Item.ArchiveRestoredAt = new(recordedAt)
				record.Item.ArchiveRestoreKnown = true
			}
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
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		var typedActor, sourceActor map[string]json.RawMessage
		if err := json.Unmarshal(fields["actor"], &typedActor); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(actorJSON), &sourceActor); err != nil {
			return err
		}
		fields["actor"] = json.RawMessage(actorJSON)
		retained, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		normalized, changed, err := normalizeCoreImportedActorJSON(retained, "actor")
		if err != nil {
			return err
		}
		if changed || !reflect.DeepEqual(typedActor, sourceActor) {
			pending[id] = normalized
		}
		record.Events = append(record.Events, event)
		return nil
	})
}

func normalizeCoreImportPageFacts(row postgres.NeutralImportRow, data *tracker.CollaborationData) error {
	var ready *bool
	if _, exists := row.Values["page_projection_ready"]; !exists {
		return nil
	}
	if err := row.Decode(map[string]any{"page_projection_ready": &ready}); err != nil {
		return err
	}
	if ready == nil || !*ready {
		return nil
	}
	var from, to, reason *string
	fields := map[string]any{}
	for name, value := range map[string]any{"from_state": &from, "to_state": &to, "reason": &reason} {
		if _, exists := row.Values[name]; exists {
			fields[name] = value
		}
	}
	if err := row.Decode(fields); err != nil {
		return err
	}
	if from != nil {
		data.FromState = *from
	}
	if to != nil {
		data.ToState = *to
	}
	if reason != nil {
		data.Reason = *reason
	}
	for _, edit := range []struct{ column, field string }{{"edited_title", "title"}, {"edited_body", "body"}} {
		if _, exists := row.Values[edit.column]; !exists {
			continue
		}
		var edited *bool
		if err := row.Decode(map[string]any{edit.column: &edited}); err != nil {
			return err
		}
		if edited == nil {
			continue
		}
		if *edited {
			if !slices.Contains(data.Fields, edit.field) {
				data.Fields = append(data.Fields, edit.field)
			}
		} else {
			data.Fields = slices.DeleteFunc(data.Fields, func(value string) bool { return value == edit.field })
		}
	}
	var run *bool
	if _, exists := row.Values["run_present"]; !exists {
		return nil
	}
	if err := row.Decode(map[string]any{"run_present": &run}); err != nil {
		return err
	}
	if run == nil {
		return nil
	}
	if !*run {
		if data.Run != nil {
			return eventstore.ErrInvalidRecord
		}
		return nil
	}
	if data.Run == nil {
		return eventstore.ErrInvalidRecord
	}
	var attempt *string
	var fence *tracker.FencingToken
	fields = map[string]any{}
	for name, value := range map[string]any{"run_attempt_id": &attempt, "run_fencing_token": &fence} {
		if _, exists := row.Values[name]; exists {
			fields[name] = value
		}
	}
	if err := row.Decode(fields); err != nil {
		return err
	}
	if attempt != nil {
		data.Run.AttemptID = *attempt
	}
	if fence != nil {
		data.Run.FencingToken = *fence
	}
	return nil
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

func coreHistoricalActivityEvidence(table string, row postgres.NeutralImportRow, timestamps ...string) (coreHistoricalActivityRow, error) {
	raw, err := json.Marshal(row)
	if err != nil {
		return coreHistoricalActivityRow{}, err
	}
	selected := postgres.NeutralImportRow{Key: append([]string(nil), row.Key...), SourceRowID: row.SourceRowID, SourcePosition: row.SourcePosition, Values: map[string]storagecatalog.Cell{}}
	for _, column := range []string{"id", "issue_id", "organization_id", "project_id", "work_item_id", "change_id", "repository_id", "native_updated_at", "updated_at", "created_at", "started_at", "recorded_at", "synchronized_at"} {
		if cell, present := row.Values[column]; present {
			selected.Values[column] = cell
		}
	}
	return coreHistoricalActivityRow{Table: table, Row: selected, RecordSHA256: artifact.Digest(raw), Timestamps: append([]string(nil), timestamps...)}, nil
}

func readCoreHistoricalActivity(ctx context.Context, bundle *postgres.VerifiedNeutralImport, organization tracker.OrganizationID) (map[string]coreHistoricalActivity, error) {
	result := map[string]coreHistoricalActivity{}
	issues := map[int64]string{}
	itemProjects := map[string]string{}
	projects := map[string]int64{}
	links := map[string][]string{}
	external := map[string][]string{}
	schema := bundle.Schema()
	visit := func(table string, fn func(postgres.NeutralImportRow) error) error {
		if _, present := schema[table]; !present {
			return nil
		}
		if err := postgres.ValidateNeutralCoreImportTables(bundle, table); err != nil {
			return err
		}
		var after []string
		for {
			rows, err := bundle.Page(ctx, table, after, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				if err := fn(row); err != nil {
					return err
				}
				after = row.Key
			}
		}
	}
	add := func(item, table string, row postgres.NeutralImportRow, values ...sql.NullString) error {
		if item == "" {
			return nil
		}
		at := time.Time{}
		for _, value := range values {
			if !value.Valid || value.String == "" {
				continue
			}
			parsed, err := time.Parse(time.RFC3339Nano, value.String)
			if err != nil {
				return errors.New("invalid historical activity timestamp")
			}
			if parsed.After(at) {
				at = parsed
			}
		}
		if at.IsZero() {
			return nil
		}
		current := result[item]
		if !at.After(current.At) {
			return nil
		}
		current.At = at
		stamps := make([]string, 0, len(values))
		for _, value := range values {
			if value.Valid && value.String != "" {
				stamps = append(stamps, value.String)
			}
		}
		evidence, err := coreHistoricalActivityEvidence(table, row, stamps...)
		if err != nil {
			return err
		}
		current.Rows = []coreHistoricalActivityRow{evidence}
		result[item] = current
		return nil
	}
	if err := visit("issues", func(row postgres.NeutralImportRow) error {
		var id int64
		var org tracker.OrganizationID
		var item, project string
		var native, updated sql.NullString
		if err := row.Decode(map[string]any{"id": &id, "organization_id": &org, "native_id": &item, "project_id": &project, "native_updated_at": &native, "updated_at": &updated}); err != nil {
			return err
		}
		if org != organization {
			return nil
		}
		issues[id] = item
		itemProjects[item] = project
		return add(item, "issues", row, native, updated)
	}); err != nil {
		return nil, err
	}
	if err := visit("projects", func(row postgres.NeutralImportRow) error {
		var org tracker.OrganizationID
		var project string
		var repository sql.NullInt64
		if err := row.Decode(map[string]any{"organization_id": &org, "id": &project, "repository_id": &repository}); err != nil {
			return err
		}
		if org == organization && repository.Valid {
			projects[project] = repository.Int64
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := visit("change_issue_links", func(row postgres.NeutralImportRow) error {
		var org tracker.OrganizationID
		var change, item string
		if err := row.Decode(map[string]any{"organization_id": &org, "change_id": &change, "work_item_id": &item}); err != nil {
			return err
		}
		if org == organization {
			links[change] = append(links[change], item)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, table := range []string{"change_requests", "change_versions", "change_evidence"} {
		if err := visit(table, func(row postgres.NeutralImportRow) error {
			var id, raw, project string
			fields := map[string]any{"record_json": &raw}
			if table == "change_requests" {
				fields["id"] = &id
				fields["project_id"] = &project
			} else {
				fields["change_id"] = &id
			}
			if err := row.Decode(fields); err != nil {
				return err
			}
			if len(links[id]) == 0 {
				return nil
			}
			var value struct {
				CreatedAt  string                           `json:"created_at"`
				ReceivedAt string                           `json:"received_at"`
				UpdatedAt  string                           `json:"updated_at"`
				External   *tracker.ChangeExternalReference `json:"external"`
			}
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				return err
			}
			stamp := value.CreatedAt
			if table == "change_requests" {
				stamp = value.UpdatedAt
			}
			if table == "change_evidence" && value.ReceivedAt != "" {
				stamp = value.ReceivedAt
			}
			for _, item := range links[id] {
				if err := add(item, table, row, sql.NullString{String: stamp, Valid: stamp != ""}); err != nil {
					return err
				}
			}
			if table == "change_versions" && value.External != nil && value.External.URL != "" {
				for _, item := range links[id] {
					if repository := projects[itemProjects[item]]; repository != 0 {
						key := strconv.FormatInt(repository, 10) + "\n" + value.External.URL
						external[key] = append(external[key], item)
					}
				}
			}

			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, table := range []string{"native_comments", "collaboration_events", "native_attempts"} {
		if err := visit(table, func(row postgres.NeutralImportRow) error {
			var item, project string
			var org tracker.OrganizationID
			var first, second sql.NullString
			fields := map[string]any{"work_item_id": &item, "project_id": &project}
			if table != "native_attempts" {
				fields["organization_id"] = &org
			}
			switch table {
			case "native_comments":
				fields["updated_at"] = &first
			case "collaboration_events":
				fields["recorded_at"] = &first
			case "native_attempts":
				fields["started_at"] = &first
				fields["updated_at"] = &second
			}
			if err := row.Decode(fields); err != nil {
				return err
			}
			if table != "native_attempts" && org != organization {
				return nil
			}
			if _, known := result[item]; !known {
				return nil
			}
			if itemProjects[item] != project {
				return corestore.ErrTenantMismatch
			}
			return add(item, table, row, first, second)
		}); err != nil {
			return nil, err
		}
	}
	if err := visit("work_events", func(row postgres.NeutralImportRow) error {
		var id int64
		var stamp sql.NullString
		if err := row.Decode(map[string]any{"issue_id": &id, "recorded_at": &stamp}); err != nil {
			return err
		}
		return add(issues[id], "work_events", row, stamp)
	}); err != nil {
		return nil, err
	}
	if err := visit("pull_requests", func(row postgres.NeutralImportRow) error {
		var issue sql.NullInt64
		var repository int64
		var url string
		var updated, synchronized sql.NullString
		if err := row.Decode(map[string]any{"issue_id": &issue, "repository_id": &repository, "url": &url, "updated_at": &updated, "synchronized_at": &synchronized}); err != nil {
			return err
		}
		targets := map[string]struct{}{}
		if issue.Valid && issues[issue.Int64] != "" {
			targets[issues[issue.Int64]] = struct{}{}
		}
		for _, item := range external[strconv.FormatInt(repository, 10)+"\n"+url] {
			targets[item] = struct{}{}
		}
		for item := range targets {
			if err := add(item, "pull_requests", row, updated, synchronized); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func importCoreHistoricalWorkItems(ctx context.Context, target *postgres.Repository, bundle *postgres.VerifiedNeutralImport, source CoreWorkItemImportSource) (int64, error) {
	mappings := map[int64]CoreWorkItemIdentityMapping{}
	targets := map[tracker.NativeWorkItemID]struct{}{}
	for _, mapping := range source.IdentityMappings {
		if mapping.SourceSHA256 != bundle.Identity().SourceSHA256 || mapping.OrganizationID != source.OrganizationID || mapping.TargetProjectID != source.ProjectID || mapping.TargetWorkItemID == "" || mapping.SourceIssueID < 1 || mapping.SourceRepositoryID < 1 || mapping.SourceGitHubNodeID == "" || mapping.SourceRepositoryNodeID == "" {
			return 0, corestore.ErrTenantMismatch
		}
		if _, exists := mappings[mapping.SourceIssueID]; exists {
			return 0, corestore.ErrRevisionConflict
		}
		if _, exists := targets[mapping.TargetWorkItemID]; exists {
			return 0, corestore.ErrRevisionConflict
		}
		mappings[mapping.SourceIssueID] = mapping
		targets[mapping.TargetWorkItemID] = struct{}{}
	}
	if len(mappings) == 0 {
		return 0, eventstore.ErrInvalidRecord
	}
	tx := newCoreNeutralImport(bundle)
	content := eventstore.NewContent(source.Objects)
	visit := func(table string, fn func(postgres.NeutralImportRow) error) error {
		if _, exists := bundle.Schema()[table]; !exists {
			return nil
		}
		var after []string
		for {
			rows, err := bundle.Page(ctx, table, after, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				if err := fn(row); err != nil {
					return err
				}
				after = row.Key
			}
		}
	}
	if _, exists := bundle.Schema()["issue_dependencies"]; exists {
		rows, err := bundle.Page(ctx, "issue_dependencies", nil, 1)
		if err != nil {
			return 0, err
		}
		if len(rows) > 0 {
			return 0, errors.New("historical dependency mappings require import before work items")
		}
	}
	historical := map[int64]coreHistoricalActivity{}
	addHistorical := func(id int64, table string, row postgres.NeutralImportRow, stamps ...string) error {
		if _, known := mappings[id]; !known {
			return corestore.ErrTenantMismatch
		}
		value := historical[id]
		atMax := value.At
		for _, stamp := range stamps {
			if stamp == "" {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				return err
			}
			if at.After(atMax) {
				atMax = at
			}
		}
		if !atMax.After(value.At) {
			return nil
		}
		value.At = atMax
		evidence, err := coreHistoricalActivityEvidence(table, row, stamps...)
		if err != nil {
			return err
		}
		value.Rows = []coreHistoricalActivityRow{evidence}
		historical[id] = value
		return nil
	}
	if err := visit("pull_requests", func(row postgres.NeutralImportRow) error {
		var issue sql.NullInt64
		var repository int64
		var updated, synchronized string
		if err := row.Decode(map[string]any{"issue_id": &issue, "repository_id": &repository, "updated_at": &updated, "synchronized_at": &synchronized}); err != nil {
			return err
		}
		if !issue.Valid {
			return nil
		}
		if mapping, known := mappings[issue.Int64]; !known || mapping.SourceRepositoryID != repository {
			return corestore.ErrTenantMismatch
		}
		return addHistorical(issue.Int64, "pull_requests", row, updated, synchronized)
	}); err != nil {
		return 0, err
	}
	if err := visit("work_events", func(row postgres.NeutralImportRow) error {
		var issue int64
		var stamp string
		if err := row.Decode(map[string]any{"issue_id": &issue, "recorded_at": &stamp}); err != nil {
			return err
		}
		return addHistorical(issue, "work_events", row, stamp)
	}); err != nil {
		return 0, err
	}
	var count int64
	err := visit("issues", func(row postgres.NeutralImportRow) error {
		var id, repositoryID, number int64
		var stateID sql.NullInt64
		var node, title, body, url, created, updated, author, labels, assignees string
		if err := row.Decode(map[string]any{"id": &id, "repository_id": &repositoryID, "github_node_id": &node, "github_number": &number, "workflow_state_id": &stateID, "title": &title, "body": &body, "url": &url, "created_at": &created, "updated_at": &updated, "author_login": &author, "labels_json": &labels, "assignees_json": &assignees}); err != nil {
			return err
		}
		mapping, known := mappings[id]
		if !known || mapping.SourceRepositoryID != repositoryID || mapping.SourceGitHubNodeID != node {
			return corestore.ErrTenantMismatch
		}
		repository, err := bundle.Lookup(ctx, "repositories", []string{strconv.FormatInt(repositoryID, 10)})
		if err != nil {
			return err
		}
		var owner, name, repositoryNode string
		if err := repository.Decode(map[string]any{"github_owner": &owner, "github_name": &name, "github_node_id": &repositoryNode}); err != nil {
			return err
		}
		if repositoryNode != mapping.SourceRepositoryNodeID {
			return corestore.ErrRevisionConflict
		}
		state := ""
		var terminal int64
		if stateID.Valid {
			workflow, err := bundle.Lookup(ctx, "workflow_states", []string{strconv.FormatInt(stateID.Int64, 10)})
			if err != nil {
				return err
			}
			var workflowRepository int64
			if err := workflow.Decode(map[string]any{"repository_id": &workflowRepository, "detent_state": &state, "terminal": &terminal}); err != nil {
				return err
			}
			if workflowRepository != repositoryID {
				return corestore.ErrTenantMismatch
			}
		}
		var projectProfile, projectRepository string
		if err := target.ReadTenant(ctx, source.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			project, err := r.ProjectRecord(ctx, mapping.TargetProjectID)
			if err != nil {
				return err
			}
			projectProfile, projectRepository = project.Core.Profile, project.Settings.Repository
			return nil
		}); err != nil {
			return err
		}
		if projectProfile != "github_compatible" || projectRepository != owner+"/"+name {
			return corestore.ErrRevisionConflict
		}

		createdAt, err := time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return err
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return err
		}
		activity := updatedAt
		evidence := []coreHistoricalActivityRow{{Table: "issues", Row: row}, {Table: "repositories", Row: repository}}
		if value, known := historical[id]; known {
			if value.At.After(activity) {
				activity = value.At
			}
			evidence = append(evidence, value.Rows...)
		}

		value := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: source.OrganizationID, ProjectID: mapping.TargetProjectID, WorkItemID: mapping.TargetWorkItemID, Number: int(number), Revision: 1, Profile: projectProfile}, Title: title, Body: body, State: state, Terminal: terminal != 0, Actor: tracker.Actor{Kind: "integration", PrincipalID: "github-import"}, Provenance: &tracker.Provenance{Provider: "github", ExternalID: node}, CreatedAt: createdAt, UpdatedAt: updatedAt, LastActivityAt: activity}
		if err := json.Unmarshal([]byte(labels), &value.Labels); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(assignees), &value.Assignees); err != nil {
			return err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: source.OrganizationID, ProjectID: mapping.TargetProjectID, WorkItemID: mapping.TargetWorkItemID, RecordID: string(mapping.TargetWorkItemID), Revision: 1}, raw)
		if err != nil {
			return err
		}
		encodedBody, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyRef, err := verifiedCoreImportObject(ctx, source.Objects, source.OrganizationID, encodedBody)
		if err != nil {
			return err
		}
		item := corestore.WorkItem{OrganizationID: source.OrganizationID, ID: id, NativeID: mapping.TargetWorkItemID, ProjectID: mapping.TargetProjectID, Number: number, Revision: 1, Title: title, BodyObjectKey: new(bodyRef.Key), ActorKind: new(value.Actor.Kind), ActorPrincipalID: new(value.Actor.PrincipalID), ProvenanceProvider: new("github"), ProvenanceExternalID: new(node), WorkflowStateName: nullStringPointer(sql.NullString{String: state, Valid: state != ""}), CreatedAt: createdAt, UpdatedAt: updatedAt, NativeCreatedAt: createdAt, NativeUpdatedAt: updatedAt, LastActivityAt: activity}
		document, err := searchstore.NewContent(source.Objects).Prepare(ctx, searchstore.Document{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.NativeID, ID: "issue:" + string(item.NativeID), SourceID: string(item.NativeID), Kind: "issue", Scope: "issue", Revision: item.Revision, UpdatedAt: value.UpdatedAt}, searchstore.Text{Title: value.Title, Body: value.Body, Labels: value.Labels})
		if err != nil {
			return err
		}
		record := postgres.CoreWorkItemImport{Item: item, Search: &document, Labels: value.Labels, Facts: claimstore.WorkItemFacts{OrganizationID: source.OrganizationID, ProjectID: mapping.TargetProjectID, WorkItemID: mapping.TargetWorkItemID, Author: author, Repository: owner + "/" + name, Assignees: value.Assignees}, Versions: []eventstore.CollaborationVersion{version}}
		if _, exists := bundle.Schema()["queue_entries"]; exists {
			if err := readCoreImportQueues(ctx, tx, source, item, &record); err != nil {
				return err
			}
		}
		sourceRows := []coreHistoricalSourceReference{}
		for _, input := range []struct {
			table string
			row   postgres.NeutralImportRow
		}{{"issues", row}, {"repositories", repository}} {
			receipt, err := prepareCoreHistoricalSourceRow(ctx, content, source.OrganizationID, bundle.Identity().SourceSHA256, id, input.table, input.row)
			if err != nil {
				return err
			}
			record.SourceReceipts = append(record.SourceReceipts, receipt)
			sourceRows = append(sourceRows, coreHistoricalSourceReference{Table: input.table, CommandKey: receipt.CommandKey})
		}
		sourceBytes, err := json.Marshal(struct {
			Mapping      CoreWorkItemIdentityMapping
			SourceRows   []coreHistoricalSourceReference
			ActivityRows []coreHistoricalActivityRow
		}{mapping, sourceRows, evidence[2:]})
		if err != nil {
			return err
		}
		receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: source.OrganizationID, ActorID: "production-import", Operation: "core-work-item-source", CommandKey: strconv.FormatInt(id, 10), RequestHash: artifact.Digest(sourceBytes)}, sourceBytes)
		if err != nil {
			return err
		}
		record.SourceReceipt = &receipt
		record.Receipt, err = prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, bundle.Identity().SourceSHA256, "core-work-item", strconv.FormatInt(id, 10), mapping)
		if err != nil {
			return err
		}
		if err := target.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		if err := target.ImportCoreWorkItem(ctx, record); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
	}
	if int(count) != len(mappings) {
		return count, corestore.ErrTenantMismatch
	}
	if err := visit("machines", func(row postgres.NeutralImportRow) error {
		var record postgres.CoreMachineImport
		record.Machine.OrganizationID = source.OrganizationID
		var registered, heartbeat, updated string
		if err := row.Decode(map[string]any{"id": &record.Machine.ID, "hostname": &record.Machine.Hostname, "display_name": &record.Machine.DisplayName, "capacity": &record.Machine.Capacity, "version": &record.Machine.Version, "registered_at": &registered, "last_heartbeat_at": &heartbeat, "updated_at": &updated}); err != nil {
			return err
		}
		var err error
		record.RegisteredAt, err = time.Parse(time.RFC3339Nano, registered)
		if err != nil {
			return err
		}
		record.LastHeartbeatAt, err = time.Parse(time.RFC3339Nano, heartbeat)
		if err != nil {
			return err
		}
		record.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return err
		}
		record.Receipt, err = prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, bundle.Identity().SourceSHA256, "core-machine", record.Machine.ID, row)
		if err != nil {
			return err
		}
		return target.ImportCoreMachine(ctx, record)
	}); err != nil {
		return count, err
	}
	if err := visit("leases", func(row postgres.NeutralImportRow) error {
		var record postgres.CoreLeaseImport
		lease := &record.Lease
		lease.OrganizationID = source.OrganizationID
		var acquired, renewed, expires string
		var released sql.NullString
		if err := row.Decode(map[string]any{"lease_id": &lease.LeaseID, "fencing_token": &lease.FencingToken, "issue_id": &lease.IssueID, "machine_id": &lease.MachineID, "session_id": &lease.SessionID, "acquired_at": &acquired, "renewed_at": &renewed, "expires_at": &expires, "released_at": &released}); err != nil {
			return err
		}
		if _, known := mappings[lease.IssueID]; !known {
			return corestore.ErrTenantMismatch
		}
		var err error
		lease.AcquiredAt, err = time.Parse(time.RFC3339Nano, acquired)
		if err != nil {
			return err
		}
		lease.RenewedAt, err = time.Parse(time.RFC3339Nano, renewed)
		if err != nil {
			return err
		}
		lease.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			return err
		}
		if released.Valid && released.String != "" {
			at, err := time.Parse(time.RFC3339Nano, released.String)
			if err != nil {
				return err
			}
			lease.ReleasedAt = &at
		}
		record.Receipt, err = prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, bundle.Identity().SourceSHA256, "core-lease", lease.LeaseID, row)
		if err != nil {
			return err
		}
		return target.ImportCoreLease(ctx, record)
	}); err != nil {
		return count, err
	}
	if err := visit("work_events", func(row postgres.NeutralImportRow) error {
		var record postgres.CoreWorkEventImport
		record.OrganizationID = source.OrganizationID
		var fence sql.NullInt64
		var machine, session, run sql.NullString
		var occurred, recorded string
		if err := row.Decode(map[string]any{"id": &record.ID, "issue_id": &record.IssueID, "fencing_token": &fence, "machine_id": &machine, "session_id": &session, "run_id": &run, "kind": &record.Kind, "payload_json": &record.PayloadJSON, "occurred_at": &occurred, "recorded_at": &recorded}); err != nil {
			return err
		}
		if _, known := mappings[record.IssueID]; !known {
			return corestore.ErrTenantMismatch
		}
		if fence.Valid {
			record.FencingToken = &fence.Int64
		}
		record.MachineID, record.SessionID, record.RunID = nullStringPointer(machine), nullStringPointer(session), nullStringPointer(run)
		var err error
		record.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred)
		if err != nil {
			return err
		}
		record.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded)
		if err != nil {
			return err
		}
		record.Payload, err = source.Objects.StoreDetail(ctx, string(source.OrganizationID), []byte(record.PayloadJSON))
		if err != nil {
			return err
		}
		record.Receipt, err = prepareCoreExecutionReceipt(ctx, content, source.OrganizationID, bundle.Identity().SourceSHA256, "core-work-event", strconv.FormatInt(record.ID, 10), row)
		if err != nil {
			return err
		}
		return target.ImportCoreWorkEvent(ctx, record)
	}); err != nil {
		return count, err
	}
	return count, nil
}
