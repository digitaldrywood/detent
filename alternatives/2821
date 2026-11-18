package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/searchsql"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) DuplicateSourceState(ctx context.Context, project tracker.ProjectID) (searchstore.DuplicateSourceState, error) {
	var state searchstore.DuplicateSourceState
	err := s.db.QueryRow(ctx, searchsql.DuplicateState, s.organization, project).Scan(&state.Sequence, &state.Ready)
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
	titleTerms, titleMinimum := searchsql.DuplicateThreshold(request.Title, true)
	bodyTerms, bodyMinimum := searchsql.DuplicateThreshold(request.Body, false)
	origins, defects, series, workerKind := []string{}, []string{}, "", " AND f.origin_kind='worker'"
	if probe, stamped := searchstore.DuplicateFingerprintProbe(query.ProjectID, request.Body); stamped {
		origins, defects, series = []string{probe.Origin}, probe.Defects, probe.ReleaseSeries
		if probe.Worker {
			workerKind = ""
		}
	}
	rows, err := s.db.Query(ctx, searchsql.Duplicates(workerKind), s.organization, query.ProjectID, origins, titleTerms, titleMinimum, bodyTerms, bodyMinimum, query.Reporter != nil, searchstore.WindowBodyChars, searchstore.WindowRows, query.Machine, query.Now.AddDate(0, 0, -30), principal, searchstore.DuplicateCandidates, defects, series, searchstore.DuplicateKeyedIssues)
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
