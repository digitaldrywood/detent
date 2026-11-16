package postgres

import (
	"context"


	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) DuplicateSourceState(ctx context.Context, project tracker.ProjectID) (searchstore.DuplicateSourceState, error) {
	var state searchstore.DuplicateSourceState
	err := s.db.QueryRow(ctx, `SELECT (p.duplicate_source_sequence+p.content_settings_revision+coalesce(s.revision,0))::bigint,
 EXISTS(SELECT 1 FROM native_work_item_page_progress g WHERE g.organization_id=p.organization_id AND g.backfill_phase>=1)
 AND NOT EXISTS(SELECT 1 FROM native_work_item_page_dirty d WHERE d.organization_id=p.organization_id AND d.project_id=p.id AND d.kind='item')
FROM projects p LEFT JOIN project_settings s ON s.organization_id=p.organization_id AND s.project_id=p.id
WHERE p.organization_id=$1 AND p.id=$2 AND p.deleted_at IS NULL`, s.organization, project).Scan(&state.Sequence, &state.Ready)
	return state, mapError(ctx, err)
}

func (s *session) LockDuplicateSource(ctx context.Context, project tracker.ProjectID, expected int64) error {
	if s.readOnly || expected < 0 || project == "" {
		return searchstore.ErrInvalid
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: project}); err != nil {
		return mapError(ctx, err)
	}
	state, err := s.DuplicateSourceState(ctx, project)
	if err != nil {
		return err
	}
	if state.Sequence != expected {
		return corestore.ErrRevisionConflict
	}
	if !state.Ready {
		return searchstore.ErrIndexPending
	}
	return nil
}

func (s *session) DuplicateSources(ctx context.Context, query searchstore.DuplicateSourceQuery) (searchstore.DuplicateSourcePage, error) {
	page := searchstore.DuplicateSourcePage{Sources: []searchstore.DuplicateSource{}}
	if query.ProjectID == "" || query.Text == "" || len(query.Text) > searchstore.MaxQueryTextBytes || query.Now.IsZero() || query.AfterID != "" || query.Reporter != nil && *query.Reporter == "" {
		return page, searchstore.ErrInvalid
	}
	state, err := s.DuplicateSourceState(ctx, query.ProjectID)
	if err != nil {
		return page, err
	}
	if query.Sequence != nil && *query.Sequence != state.Sequence {
		return page, corestore.ErrRevisionConflict
	}
	page.State = state
	if !state.Ready {
		return page, searchstore.ErrIndexPending
	}
	principal := ""
	if query.Reporter != nil {
		principal = *query.Reporter
	}
	request := searchstore.Text{Title: query.Text}
	if query.Request != nil {
		request = *query.Request
	}
	titleTerms, titleMinimum := duplicateThreshold(request.Title, true)
	bodyTerms, bodyMinimum := duplicateThreshold(request.Body, false)
	keys := searchstore.DuplicateFingerprintKeys(request.Body, false)
	if keys == nil {
		keys = []string{}
	}
	rows, err := s.db.Query(ctx, `WITH window_items AS MATERIALIZED (
 SELECT work_item_id,number,revision,title,left(body,$9) AS body,search_vector,terminal,archived,closed_at,created_at
 FROM native_work_item_pages WHERE organization_id=$1 AND project_id=$2 ORDER BY number DESC,work_item_id LIMIT $10
), matched AS MATERIALIZED (
 SELECT w.*,EXISTS(SELECT 1 FROM unnest($3::text[]) k WHERE position(k IN w.body)>0) AS fingerprint_hit,
 (SELECT count(*) FROM unnest($4::text[]) term WHERE w.search_vector @@ to_tsquery('pg_catalog.english',term)) AS title_hits,
 (SELECT count(*) FROM unnest($6::text[]) term WHERE w.search_vector @@ to_tsquery('pg_catalog.english',term)) AS body_hits
 FROM window_items w WHERE NOT w.archived AND ($11::boolean OR NOT w.terminal OR coalesce(w.closed_at,w.created_at)>=$12)
)
SELECT m.work_item_id,m.number,m.revision,m.title,m.body,m.terminal,i.workflow_state_name,m.fingerprint_hit
FROM matched m JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=m.work_item_id AND i.revision=m.revision AND NOT i.archived
WHERE (m.fingerprint_hit OR (cardinality($4::text[])>0 AND m.title_hits>=$5) OR (cardinality($6::text[])>0 AND m.body_hits>=$7))
 AND (NOT $8::boolean OR i.actor_principal_id=$13)
ORDER BY m.fingerprint_hit DESC,greatest(m.title_hits,m.body_hits) DESC,m.number DESC,m.work_item_id
LIMIT $14`, s.organization, query.ProjectID, keys, titleTerms, titleMinimum, bodyTerms, bodyMinimum, query.Reporter != nil, searchstore.WindowBodyChars, searchstore.WindowRows, query.Machine, query.Now.AddDate(0, 0, -30), principal, searchstore.DuplicateCandidates)
	if err != nil {
		return page, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var source searchstore.DuplicateSource
		var item string
		var revision int64
		var state *string
		var fingerprint bool
		if err := rows.Scan(&item, &source.Number, &revision, &source.Text.Title, &source.Text.Body, &source.Terminal, &state, &fingerprint); err != nil {
			return page, mapError(ctx, err)
		}
		if state != nil {
			source.State = *state
		}
		source.Revision = tracker.Revision(revision)
		source.Document = searchstore.Document{OrganizationID: s.organization, ProjectID: query.ProjectID, WorkItemID: tracker.NativeWorkItemID(item), ID: "issue:" + item, SourceID: item, Kind: "issue", Scope: "issue", Revision: revision}
		page.Sources = append(page.Sources, source)
	}
	if err := rows.Err(); err != nil {
		return page, mapError(ctx, err)
	}
	page.Exhausted = true
	current, err := s.DuplicateSourceState(ctx, query.ProjectID)
	if err != nil {
		return page, err
	}
	if current != state {
		return searchstore.DuplicateSourcePage{}, corestore.ErrRevisionConflict
	}
	return page, nil
}

func (s *session) lockDuplicateSourceProject(ctx context.Context, project tracker.ProjectID, kind string) error {
	if project == "" || kind == "thread" {
		return nil
	}
	_, err := s.queries.DuplicateProjectLock(ctx, pgquery.DuplicateProjectLockParams{OrganizationID: s.organization, ID: project})
	return mapError(ctx, err)
}

func (s *session) lockDuplicateSourceDocument(ctx context.Context, id string) error {
	identity, err := s.queries.DuplicateDocumentIdentity(ctx, pgquery.DuplicateDocumentIdentityParams{OrganizationID: s.organization, ID: id})
	if err != nil {
		return mapError(ctx, err)
	}
	if identity.ProjectID == nil {
		return nil
	}
	return s.lockDuplicateSourceProject(ctx, *identity.ProjectID, identity.Kind)
}

func duplicateThreshold(content string, title bool) ([]string, int64) {
	terms := searchstore.DuplicateWindowTerms(content)
	minimum := max(2, (len(terms)*3+4)/5)
	if title && len(terms) == 1 {
		minimum = 1
	}
	return terms, int64(minimum)
}

func (s *session) advanceDuplicateSource(ctx context.Context, project tracker.ProjectID) error {
	count, err := s.queries.AdvanceDuplicateSource(ctx, pgquery.AdvanceDuplicateSourceParams{OrganizationID: s.organization, ID: project})
	return affected(ctx, count, err)
}
