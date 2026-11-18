package projectreads

import (
	"context"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type projectWorkflow struct {
	project  pgquery.Project
	settings pgquery.ProjectSetting
	states   []tracker.NativeState
}

type workflowKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

type versionedStates struct {
	revision        int64
	contentRevision int64
	states          []tracker.NativeState
}

type workflowReads struct {
	pool    *pgxpool.Pool
	mu      sync.Mutex
	entries map[workflowKey]versionedStates
}

func (reads *workflowReads) get(key workflowKey) (versionedStates, bool) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	entry, ok := reads.entries[key]
	return entry, ok
}

func (reads *workflowReads) remember(key workflowKey, record projectWorkflow) {
	reads.mu.Lock()
	defer reads.mu.Unlock()
	if reads.entries == nil {
		reads.entries = make(map[workflowKey]versionedStates)
	}
	reads.entries[key] = versionedStates{revision: record.settings.Revision, contentRevision: record.project.ContentSettingsRevision, states: record.states}
}

type workflowReadsKey struct{}

type Reader struct {
	scope    *workflowReads
	snapshot map[tracker.ProjectID]projectWorkflow
	readOnly bool
}

func WithScope(ctx context.Context, pool *pgxpool.Pool) context.Context {
	return context.WithValue(ctx, workflowReadsKey{}, &workflowReads{pool: pool})
}

func New(ctx context.Context, pool *pgxpool.Pool, readOnly bool) Reader {
	scope, _ := ctx.Value(workflowReadsKey{}).(*workflowReads)
	if scope != nil && scope.pool != pool {
		scope = nil
	}
	return Reader{scope: scope, readOnly: readOnly}
}

func (r *Reader) BeginSnapshot() {
	r.snapshot = make(map[tracker.ProjectID]projectWorkflow)
}

func (r *Reader) Record(ctx context.Context, q *pgquery.Queries, org tracker.OrganizationID, id tracker.ProjectID) (projectstore.Record, error) {
	record, found := r.snapshot[id]
	if !found {
		var err error
		record, err = r.readWorkflow(ctx, q, org, id)
		if err != nil {
			return projectstore.Record{}, err
		}
		if r.snapshot != nil {
			r.snapshot[id] = record
		}
	}
	states := slices.Clone(record.states)
	for i := range states {
		states[i].Transitions = slices.Clone(states[i].Transitions)
	}
	return projectstore.Record{Core: Core(record.project), Settings: projectSettings(record.settings), States: states}, nil
}

func (r *Reader) readWorkflow(ctx context.Context, q *pgquery.Queries, org tracker.OrganizationID, id tracker.ProjectID) (projectWorkflow, error) {
	key := workflowKey{organization: org, project: id}
	if r.scope != nil {
		if known, ok := r.scope.get(key); ok {
			meta, err := q.BoundProjectMetadata(ctx, pgquery.BoundProjectMetadataParams{OrganizationID: org, ID: id})
			if err != nil {
				return projectWorkflow{}, pgcore.MapError(ctx, err)
			}
			if meta.ProjectSetting.Revision == known.revision && meta.Project.ContentSettingsRevision == known.contentRevision {
				return projectWorkflow{project: meta.Project, settings: meta.ProjectSetting, states: known.states}, nil
			}
		}
	}
	rows, err := q.BoundProjectRecord(ctx, pgquery.BoundProjectRecordParams{OrganizationID: org, ID: id})
	if err != nil {
		return projectWorkflow{}, pgcore.MapError(ctx, err)
	}
	if len(rows) == 0 {
		return projectWorkflow{}, corestore.ErrNotFound
	}
	states := []tracker.NativeState{}
	positions := make(map[string]int)
	for _, row := range rows {
		if row.StateName == nil {
			continue
		}
		i, found := positions[*row.StateName]
		if !found {
			i = len(states)
			positions[*row.StateName] = i
			states = append(states, tracker.NativeState{Name: *row.StateName, Terminal: row.StateTerminal, Dispatchable: row.StateDispatchable, OperatorOnly: row.StateOperatorOnly, Transitions: []string{}})
		}
		if row.TargetName != nil {
			states[i].Transitions = append(states[i].Transitions, *row.TargetName)
		}
	}
	record := projectWorkflow{project: rows[0].Project, settings: rows[0].ProjectSetting, states: states}
	if r.readOnly && r.scope != nil {
		r.scope.remember(key, record)
	}
	return record, nil
}

func projectSettings(row pgquery.ProjectSetting) projectstore.Settings {
	result := projectstore.Settings{Icon: projecticon.Icon{Kind: row.IconKind, Name: row.IconName, Color: row.IconColor, Emoji: row.IconEmoji, ImageKey: row.IconImageKey, ImageSHA256: row.IconImageSha256}, OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Revision: row.Revision, Intake: row.Intake, Projection: row.Projection, RepositoryEnabled: row.RepositoryEnabled, Repository: row.Repository, CheckoutRepository: row.CheckoutRepository, AgingEnabled: row.AgingEnabled, DuplicateChallenge: row.DuplicateChallenge, WorkflowKey: row.WorkflowKey, WorkflowSHA256: row.WorkflowSha256, WorkflowBytes: row.WorkflowBytes, WorkflowContentBytes: row.WorkflowContentBytes, RequireVisualAttachments: row.RequireVisualAttachments, ModelRevision: row.ModelRevision, ModelKey: row.ModelKey, ModelSHA256: row.ModelSha256, ModelBytes: row.ModelBytes, ModelContentBytes: row.ModelContentBytes}
	if row.ArchiveCompletedAfterDays.Valid {
		result.ArchiveCompletedAfterDays = new(row.ArchiveCompletedAfterDays.Int64)
	}
	if row.ArchiveCancelledAfterDays.Valid {
		result.ArchiveCancelledAfterDays = new(row.ArchiveCancelledAfterDays.Int64)
	}
	return result
}

func (r *Reader) Metadata(ctx context.Context, q *pgquery.Queries, org tracker.OrganizationID, id tracker.ProjectID) (projectstore.Metadata, error) {
	if known, ok := r.snapshot[id]; ok {
		return projectstore.Metadata{Core: Core(known.project), Settings: projectSettings(known.settings)}, nil
	}
	row, err := q.BoundProjectMetadata(ctx, pgquery.BoundProjectMetadataParams{OrganizationID: org, ID: id})
	if err != nil {
		return projectstore.Metadata{}, pgcore.MapError(ctx, err)
	}
	return projectstore.Metadata{Core: Core(row.Project), Settings: projectSettings(row.ProjectSetting)}, nil
}

func (r *Reader) Version(ctx context.Context, q *pgquery.Queries, org tracker.OrganizationID, id tracker.ProjectID) (projectstore.Version, error) {
	if known, ok := r.snapshot[id]; ok {
		return projectstore.Version{Slug: known.project.Slug, SettingsRevision: known.settings.Revision, ContentSettingsRevision: known.project.ContentSettingsRevision, ModelRevision: known.settings.ModelRevision}, nil
	}
	row, err := q.BoundProjectVersion(ctx, pgquery.BoundProjectVersionParams{OrganizationID: org, ID: id})
	return projectstore.Version{Slug: row.Slug, SettingsRevision: row.Revision, ContentSettingsRevision: row.ContentSettingsRevision, ModelRevision: row.ModelRevision}, pgcore.MapError(ctx, err)
}

func Core(row pgquery.Project) corestore.Project {
	return corestore.Project{
		OrganizationID:          row.OrganizationID,
		ID:                      row.ID,
		Name:                    row.Name,
		Slug:                    row.Slug,
		Profile:                 row.Profile,
		RequireDependencies:     row.RequireDependencies,
		SchedulingRank:          row.SchedulingRank,
		ContentSettingsRevision: row.ContentSettingsRevision,
		WorkflowSource:          row.WorkflowSource,
		WorkflowSourceRevision:  row.WorkflowSourceRevision,
		WorkflowObjectKey:       row.WorkflowObjectKey,
		StatesObjectKey:         row.StatesObjectKey,
		ModelSelectionObjectKey: row.ModelSelectionObjectKey,
		CreatedAt:               row.CreatedAt,
		DeletedAt:               row.DeletedAt,
	}
}
