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
 NOT EXISTS(SELECT 1 FROM native_work_item_page_progress g WHERE g.organization_id=p.organization_id AND g.backfill_phase=0)
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
	origins, defects, series, workerKind := []string{}, []string{}, "", " AND f.origin_kind='worker'"
	if probe, stamped := searchstore.DuplicateFingerprintProbe(query.ProjectID, request.Body); stamped {
		origins, defects, series = []string{probe.Origin}, probe.Defects, probe.ReleaseSeries
		if probe.Worker {
			workerKind = ""
		}
	}
	scan := func(name, digests, predicate string) string {
		next := func(after string) string {
			return "(SELECT f.work_item_id FROM native_work_item_fingerprints f WHERE f.organization_id=$1 AND f.project_id=$2 AND f.fingerprint_digest=" + name + "_key.digest AND f.release_series=$16" + predicate + after + " ORDER BY f.work_item_id LIMIT 1)"
		}
		return name + "(digest,work_item_id,steps) AS (\n SELECT " + name + "_key.digest," + next("") + ",1 FROM unnest(" + digests + "::text[]) " + name + "_key(digest)\n UNION ALL\n SELECT " + name + "_key.digest," + next(" AND f.work_item_id>"+name+"_key.work_item_id") + "," + name + "_key.steps+1 FROM " + name + " " + name + "_key WHERE " + name + "_key.work_item_id IS NOT NULL AND " + name + "_key.steps<$17\n)"
	}
	rows, err := s.db.Query(ctx, `WITH RECURSIVE recent AS MATERIALIZED (
 SELECT work_item_id FROM native_work_item_pages WHERE organization_id=$1 AND project_id=$2 ORDER BY number DESC,work_item_id LIMIT $10
), `+scan("origin_scan", "$3", " AND f.is_origin")+`, `+scan("defect_scan", "$15", workerKind)+`, keyed AS MATERIALIZED (
 SELECT work_item_id FROM origin_scan WHERE work_item_id IS NOT NULL
 UNION SELECT work_item_id FROM defect_scan WHERE work_item_id IS NOT NULL
), window_items AS MATERIALIZED (
 SELECT p.work_item_id,p.number,p.revision,p.search_vector,p.terminal,p.archived,p.closed_at,p.created_at,k.work_item_id IS NOT NULL AS keyed
 FROM native_work_item_pages p LEFT JOIN keyed k ON k.work_item_id=p.work_item_id
 WHERE p.organization_id=$1 AND p.project_id=$2 AND p.work_item_id IN (SELECT work_item_id FROM recent UNION SELECT work_item_id FROM keyed)
), title_queries AS MATERIALIZED (
 SELECT to_tsquery('pg_catalog.english',term) AS q FROM unnest($4::text[]) term
), body_queries AS MATERIALIZED (
 SELECT to_tsquery('pg_catalog.english',term) AS q FROM unnest($6::text[]) term
), matched AS MATERIALIZED (
 SELECT w.*,
 (SELECT count(*) FROM title_queries t WHERE w.search_vector @@ t.q) AS title_hits,
 (SELECT count(*) FROM body_queries b WHERE w.search_vector @@ b.q) AS body_hits
 FROM window_items w WHERE NOT w.archived AND ($11::boolean OR NOT w.terminal OR coalesce(w.closed_at,w.created_at)>=$12)
)
SELECT c.work_item_id,c.number,c.revision,p.title,left(p.body,$9),c.terminal,c.workflow_state_name,c.keyed FROM (
 SELECT m.work_item_id,m.number,m.revision,m.terminal,i.workflow_state_name,m.keyed,greatest(m.title_hits,m.body_hits) AS hits,
  row_number() OVER (ORDER BY greatest(m.title_hits,m.body_hits) DESC,m.number DESC,m.work_item_id) AS term_rank,
  row_number() OVER (PARTITION BY m.keyed ORDER BY m.number DESC,m.work_item_id) AS keyed_rank
 FROM matched m JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=m.work_item_id AND i.revision=m.revision AND NOT i.archived
 WHERE (m.keyed OR (cardinality($4::text[])>0 AND m.title_hits>=$5) OR (cardinality($6::text[])>0 AND m.body_hits>=$7))
  AND (NOT $8::boolean OR i.actor_principal_id=$13)
) c JOIN native_work_item_pages p ON p.organization_id=$1 AND p.project_id=$2 AND p.work_item_id=c.work_item_id
WHERE (c.keyed AND c.keyed_rank<=$14) OR c.term_rank<=$14
ORDER BY c.keyed DESC,c.hits DESC,c.number DESC,c.work_item_id`, s.organization, query.ProjectID, origins, titleTerms, titleMinimum, bodyTerms, bodyMinimum, query.Reporter != nil, searchstore.WindowBodyChars, searchstore.WindowRows, query.Machine, query.Now.AddDate(0, 0, -30), principal, searchstore.DuplicateCandidates, defects, series, searchstore.DuplicateKeyedIssues)
	if err != nil {
		return page, mapError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var source searchstore.DuplicateSource
		var item string
		var revision int64
		var state *string
		if err := rows.Scan(&item, &source.Number, &revision, &source.Text.Title, &source.Text.Body, &source.Terminal, &state, &source.Fingerprint); err != nil {
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

func (s *session) lockDuplicateSourceProject(ctx context.Context, project tracker.ProjectID, kind string) error {
	if project == "" || kind == "thread" {
		return nil
	}
	_, err := s.queries.DuplicateProjectLock(ctx, pgquery.DuplicateProjectLockParams{OrganizationID: s.organization, ID: project})
	return mapError(ctx, err)
}
