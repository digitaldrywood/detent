package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) DuplicateSourceState(ctx context.Context, project tracker.ProjectID) (searchstore.DuplicateSourceState, error) {
	row, err := s.queries.DuplicateSourceState(ctx, pgquery.DuplicateSourceStateParams{OrganizationID: s.organization, ProjectID: project})
	return searchstore.DuplicateSourceState{Sequence: row.SourceSequence, Ready: row.Ready}, mapError(ctx, err)
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
	expression := ""
	searchQuery := searchstore.Query{ProjectID: query.ProjectID, Text: query.Text, Exact: query.Exact}
	if query.Exact {
		expression = searchstore.LiteralExpression(s.organization, searchQuery)
	} else {
		words := searchstore.Expressions(s.organization, searchQuery)
		if len(words) > 0 {
			expression = "(" + strings.Join(words, " | ") + ")"
		}
		if literal := searchstore.LiteralExpression(s.organization, searchQuery); literal != "" {
			if expression != "" {
				expression += " | "
			}
			expression += "(" + literal + ")"
		}
	}
	if expression == "" {
		return page, searchstore.ErrInvalid
	}
	request := searchstore.Text{Title: query.Text}
	if query.Request != nil {
		request = *query.Request
	}
	titleExpressions, titleMinimum := duplicateThreshold(s.organization, query.ProjectID, request.Title, true)
	bodyExpressions, bodyMinimum := duplicateThreshold(s.organization, query.ProjectID, request.Body, false)
	if !query.Exact {
		for _, terms := range [][]string{titleExpressions, bodyExpressions} {
			if len(terms) > 0 {
				expression = "(" + expression + ") | (" + strings.Join(terms, " | ") + ")"
			}
		}
	}
	literal := searchstore.LiteralExpression(s.organization, searchQuery)
	fingerprint := ""
	if origin, ok := issueorigin.Parse(request.Body); ok {
		values := searchstore.Expressions(s.organization, searchstore.Query{ProjectID: query.ProjectID, Text: origin.Fingerprint + " " + issueorigin.DefectFingerprint(request.Body), Exact: true})
		if len(values) > 0 {
			fingerprint = strings.Join(values, " | ")
			expression = "(" + expression + ") | (" + fingerprint + ")"
		}
	}
	rows, err := s.queries.DuplicateSources(ctx, pgquery.DuplicateSourcesParams{Exact: query.Exact, Machine: query.Machine, LiteralExpression: literal, FingerprintExpression: fingerprint, TitleExpressions: titleExpressions, TitleMinimum: titleMinimum, BodyExpressions: bodyExpressions, BodyMinimum: bodyMinimum, Expression: expression, OrganizationID: s.organization, ProjectID: new(query.ProjectID), Reporter: query.Reporter != nil, PrincipalID: principal, TerminalAfter: query.Now.AddDate(0, 0, -30)})
	if err != nil {
		return page, mapError(ctx, err)
	}
	page.Exhausted = true
	parents := make(map[tracker.NativeWorkItemID]searchstore.Document)
	for _, row := range rows {
		document := searchDocument(row.SearchDocument)
		parent := document
		if document.Kind == "comment" {
			var found bool
			parent, found = parents[document.WorkItemID]
			if !found {
				record, err := s.queries.DuplicateParentSource(ctx, pgquery.DuplicateParentSourceParams{OrganizationID: s.organization, ProjectID: new(query.ProjectID), SourceID: string(document.WorkItemID)})
				if errors.Is(err, pgx.ErrNoRows) {
					return searchstore.DuplicateSourcePage{State: searchstore.DuplicateSourceState{Sequence: state.Sequence}}, searchstore.ErrIndexPending
				}
				if err != nil {
					return searchstore.DuplicateSourcePage{}, mapError(ctx, err)
				}
				if record.Revision != row.IssueRevision {
					return searchstore.DuplicateSourcePage{State: searchstore.DuplicateSourceState{Sequence: state.Sequence}}, searchstore.ErrIndexPending
				}
				parent = searchDocument(record)
				parents[document.WorkItemID] = parent
			}
		}
		page.Sources = append(page.Sources, searchstore.DuplicateSource{Parent: parent, Document: duplicateCandidateDocument(row.SearchDocument, query.Machine), Statistics: searchstore.Statistics{Tokens: row.SourceTokens, Terms: row.SourceTerms, Frequencies: row.SourceFrequencies}, Revision: tracker.Revision(row.IssueRevision), Number: row.IssueNumber, State: row.IssueState, Terminal: row.Terminal})
	}
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

func duplicateThreshold(organization tracker.OrganizationID, project tracker.ProjectID, content string, title bool) ([]string, int64) {
	terms := searchstore.DuplicateQueryExpressions(organization, project, content)
	minimum := max(2, (len(terms)*3+4)/5)
	if title && len(terms) == 1 {
		minimum = 1
	}
	return terms, int64(minimum)
}

func duplicateCandidateDocument(row pgquery.SearchDocument, machine bool) searchstore.Document {
	document := searchDocument(row)
	if !machine && row.Kind == "issue" && row.AggregateObjectKey != nil && row.AggregateSha256 != nil && row.AggregateBytes.Valid && row.AggregateContentBytes.Valid {
		document.Payload.Key = *row.AggregateObjectKey
		document.Payload.SHA256 = *row.AggregateSha256
		document.Payload.CompressedSize = row.AggregateBytes.Int64
		document.Payload.UncompressedSize = row.AggregateContentBytes.Int64
	}
	return document
}

func (s *session) advanceDuplicateSource(ctx context.Context, project tracker.ProjectID) error {
	count, err := s.queries.AdvanceDuplicateSource(ctx, pgquery.AdvanceDuplicateSourceParams{OrganizationID: s.organization, ID: project})
	return affected(ctx, count, err)
}
