package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/searchsql"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) WorkItemDetailPage(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (viewstore.ItemPageRecord, error) {
	row, err := scanItemPage(s.db.QueryRow(ctx, "SELECT "+itemPageColumns+" FROM native_work_item_pages WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", s.organization, project, item))
	return row, mapError(ctx, err)
}

func (s *session) CommentPageRecords(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, newest bool, limit int) ([]viewstore.CommentPageRecord, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	ready, err := s.WorkItemPageReady(ctx, project, item, "comment")
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, viewstore.ErrPagePending
	}
	order, predicate := "ASC", ">"
	if newest {
		order, predicate = "DESC", "<"
	}
	rows, err := s.db.Query(ctx, "SELECT "+commentPageColumns+" FROM native_comment_pages WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND sequence"+predicate+"$4 ORDER BY sequence "+order+" LIMIT $5", s.organization, project, item, after, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.CommentPageRecord{}
	for rows.Next() {
		row, err := scanCommentPage(rows)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) NextWorkItemPageCandidate(ctx context.Context) (viewstore.PageCandidate, error) {
	row := viewstore.PageCandidate{OrganizationID: s.organization}
	err := s.db.QueryRow(ctx, `WITH position AS MATERIALIZED (
 SELECT COALESCE((SELECT cursor_generation FROM native_work_item_page_progress WHERE organization_id=$1),0) AS generation
 ), candidates AS (
 (SELECT d.* FROM native_work_item_page_dirty d,position p WHERE d.organization_id=$1 AND d.generation>p.generation ORDER BY d.generation LIMIT 1)
 UNION ALL
 (SELECT d.* FROM native_work_item_page_dirty d,position p WHERE d.organization_id=$1 AND d.generation<=p.generation ORDER BY d.generation LIMIT 1)
 ) SELECT candidates.project_id,candidates.work_item_id,candidates.record_id,candidates.kind,candidates.generation,candidates.source_at,candidates.after_type,candidates.after_direction,candidates.after_project,candidates.after_item,candidates.propagate,candidates.cleaning FROM candidates,position
 ORDER BY (candidates.generation>position.generation) DESC,candidates.generation LIMIT 1`, s.organization).Scan(&row.ProjectID, &row.WorkItemID, &row.RecordID, &row.Kind, &row.Generation, &row.SourceAt, &row.After.Type, &row.After.Direction, &row.After.RelatedProjectID, &row.After.RelatedWorkItemID, &row.Propagate, &row.Cleaning)
	return row, mapError(ctx, err)
}

func (s *session) AdvanceWorkItemPageCursor(ctx context.Context, candidate viewstore.PageCandidate) error {
	if err := s.checkOrganization(candidate.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_progress (organization_id,cursor_generation) VALUES ($1,$2)
 ON CONFLICT (organization_id) DO UPDATE SET cursor_generation=EXCLUDED.cursor_generation,examined_at=clock_timestamp()`, s.organization, candidate.Generation)
	return mapError(ctx, err)
}

func (s *session) PublishWorkItemPage(ctx context.Context, candidate viewstore.PageCandidate, item *viewstore.ItemPageRecord, comment *viewstore.CommentPageRecord) error {
	if err := s.checkOrganization(candidate.OrganizationID); err != nil {
		return err
	}
	if candidate.Generation < 1 || candidate.Kind != "item" && candidate.Kind != "comment" {
		return corestore.ErrInvalidPage
	}
	if item != nil && (candidate.Kind != "item" || item.Generation != candidate.Generation || item.Value.OrganizationID != s.organization || item.Value.ProjectID != candidate.ProjectID || item.Value.WorkItemID != candidate.WorkItemID) {
		return corestore.ErrTenantMismatch
	}
	if comment != nil && (candidate.Kind != "comment" || comment.Generation != candidate.Generation || comment.Value.OrganizationID != s.organization || comment.Value.ProjectID != candidate.ProjectID || comment.Value.WorkItemID != candidate.WorkItemID || comment.Value.ID != candidate.RecordID) {
		return corestore.ErrTenantMismatch
	}
	var fingerprints []searchstore.FingerprintRow
	if item != nil {
		fingerprints = searchstore.FingerprintRows(candidate.ProjectID, item.Value.Body)
	} else if comment != nil && !comment.Value.Deleted {
		fingerprints = searchstore.FingerprintRows(candidate.ProjectID, comment.Value.Body)
	}
	var stored bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM native_work_item_fingerprints WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind=$4 AND record_id=$5)`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Kind, candidate.RecordID).Scan(&stored); err != nil {
		return mapError(ctx, err)
	}
	if stored || len(fingerprints) > 0 {
		if _, err := s.queries.AdvanceDuplicateSource(ctx, pgquery.AdvanceDuplicateSourceParams{OrganizationID: s.organization, ID: candidate.ProjectID}); err != nil {
			return mapError(ctx, err)
		}
	}
	var generation int64
	err := s.db.QueryRow(ctx, `SELECT generation FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$4 AND kind=$5 FOR UPDATE`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.RecordID, candidate.Kind).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapError(ctx, err)
	}
	if generation != candidate.Generation {
		return nil
	}
	if item != nil {
		if err := s.putItemPage(ctx, *item); err != nil {
			return err
		}
		if err := s.QueueRelationshipPage(ctx, candidate.ProjectID, candidate.WorkItemID, true); err != nil {
			return err
		}
	} else if comment != nil {
		if err := s.putCommentPage(ctx, *comment); err != nil {
			return err
		}
	} else {
		table := "native_work_item_pages"
		predicate := ""
		if candidate.Kind == "comment" {
			table = "native_comment_pages"
			predicate = " AND comment_id=$4"
		}
		args := []any{s.organization, candidate.ProjectID, candidate.WorkItemID}
		if candidate.Kind == "comment" {
			args = append(args, candidate.RecordID)
		}
		if _, err := s.db.Exec(ctx, "DELETE FROM "+table+" WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3"+predicate, args...); err != nil {
			return mapError(ctx, err)
		}
		if candidate.Kind == "item" {
			if _, err := s.db.Exec(ctx, searchsql.TermsDelete, args...); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	if err := s.replaceWorkItemFingerprints(ctx, candidate, fingerprints); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$4 AND kind=$5 AND generation=$6`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.RecordID, candidate.Kind, candidate.Generation)
	if err != nil {
		return mapError(ctx, err)
	}
	return s.advanceWorkItemPageProjectionRevision(ctx)
}

func (s *session) WorkItemReferenceRecords(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) ([]tracker.ExternalReference, error) {
	var providers, kinds, ids, urls, repositories []string
	var numbers []int
	err := s.db.QueryRow(ctx, `SELECT reference_providers,reference_kinds,reference_ids,reference_urls,reference_repositories,reference_numbers FROM native_work_item_pages WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, s.organization, project, item).Scan(&providers, &kinds, &ids, &urls, &repositories, &numbers)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	references := make([]tracker.ExternalReference, len(providers))
	for i, provider := range providers {
		references[i] = tracker.ExternalReference{Provider: provider, Kind: kinds[i], ID: ids[i], URL: urls[i], Repository: repositories[i], Number: numbers[i]}
	}
	return references, nil
}

func (s *session) WorkItemPageProjectionRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE((SELECT publication_revision FROM native_work_item_page_progress WHERE organization_id=$1),0)`, s.organization).Scan(&revision)
	return revision, mapError(ctx, err)
}
func (s *session) advanceWorkItemPageProjectionRevision(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_progress(organization_id,publication_revision) VALUES($1,1)
 ON CONFLICT(organization_id) DO UPDATE SET publication_revision=native_work_item_page_progress.publication_revision+1`, s.organization)
	return mapError(ctx, err)
}

func (s *session) replaceWorkItemFingerprints(ctx context.Context, candidate viewstore.PageCandidate, rows []searchstore.FingerprintRow) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM native_work_item_fingerprints WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind=$4 AND record_id=$5`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Kind, candidate.RecordID); err != nil {
		return mapError(ctx, err)
	}
	if len(rows) == 0 {
		return nil
	}
	digests, origins, kinds, series := make([]string, len(rows)), make([]bool, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for i, row := range rows {
		digests[i], origins[i], kinds[i], series[i] = row.Digest, row.Origin, row.OriginKind, row.ReleaseSeries
	}
	_, err := s.db.Exec(ctx, `INSERT INTO native_work_item_fingerprints(organization_id,project_id,work_item_id,kind,record_id,fingerprint_digest,is_origin,origin_kind,release_series)
 SELECT $1,$2,$3,$4,$5,f.digest,f.origin,f.kind,f.series FROM unnest($6::text[],$7::boolean[],$8::text[],$9::text[]) AS f(digest,origin,kind,series)`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Kind, candidate.RecordID, digests, origins, kinds, series)
	return mapError(ctx, err)
}

func (s *session) WorkItemPageReady(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, kind string) (bool, error) {
	var ready bool
	err := s.db.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM native_work_item_page_progress WHERE organization_id=$1 AND backfill_phase=2)
 AND NOT EXISTS(SELECT 1 FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND kind=$4 AND NOT cleaning)
 AND CASE WHEN $4='item' THEN EXISTS(SELECT 1 FROM native_work_item_pages WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)
 WHEN $4='relationships' THEN EXISTS(SELECT 1 FROM native_relationship_page_heads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3)
 ELSE true END`, s.organization, project, item, kind).Scan(&ready)
	return ready, mapError(ctx, err)
}
