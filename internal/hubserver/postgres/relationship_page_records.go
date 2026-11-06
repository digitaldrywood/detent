package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) QueueRelationshipPage(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, propagate bool) error {
	_, err := s.db.Exec(ctx, `INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind,propagate) VALUES($1,$2,$3,$3,'relationships',$4)
 ON CONFLICT(organization_id,project_id,work_item_id,record_id,kind) DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp(),after_type='',after_direction='',after_project='',after_item='',cleaning=false,propagate=native_work_item_page_dirty.propagate OR EXCLUDED.propagate`, s.organization, project, item, propagate)
	return mapError(ctx, err)
}

func (s *session) RelationshipPageRecords(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after corestore.RelationshipCursor, limit int) ([]viewstore.RelationshipPageRecord, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT COALESCE(p.relationship_type,''),COALESCE(p.direction,''),COALESCE(p.related_project_id,''),COALESCE(p.related_work_item_id,''),COALESCE(p.number,0),COALESCE(p.title,''),COALESCE(p.state,''),COALESCE(p.terminal,false),COALESCE(p.dispatchable,false),p.actor_principal_id,COALESCE(p.web_url,''),COALESCE(p.identifier,''),p.closed_at,p.started_at
 FROM native_relationship_page_heads h LEFT JOIN LATERAL (
 SELECT p.* FROM native_relationship_pages p WHERE p.organization_id=h.organization_id AND p.project_id=h.project_id AND p.work_item_id=h.work_item_id AND p.generation=h.generation
 AND (relationship_type,direction,related_project_id,related_work_item_id)>($4,$5,$6,$7)
 ORDER BY relationship_type,direction,related_project_id,related_work_item_id LIMIT $8
 ) p ON true WHERE h.organization_id=$1 AND h.project_id=$2 AND h.work_item_id=$3
 ORDER BY p.relationship_type,p.direction,p.related_project_id,p.related_work_item_id`, s.organization, project, item, after.Type, after.Direction, after.RelatedProjectID, after.RelatedWorkItemID, limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []viewstore.RelationshipPageRecord{}
	found := false
	for rows.Next() {
		found = true
		record := viewstore.RelationshipPageRecord{Relationship: corestore.Relationship{OrganizationID: s.organization, ProjectID: project, WorkItemID: item}}
		r := &record.Relationship
		if err := rows.Scan(&r.Type, &r.Direction, &r.RelatedProjectID, &r.RelatedWorkItemID, &r.Number, &r.Title, &r.State, &r.Terminal, &r.Dispatchable, &r.ActorPrincipalID, &record.WebURL, &record.Identifier, &r.ClosedAt, &r.StartedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		if r.Type != "" {
			result = append(result, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if !found {
		return nil, corestore.ErrNotFound
	}
	return result, nil
}

func (s *session) PublishRelationshipPage(ctx context.Context, candidate viewstore.PageCandidate, records []viewstore.RelationshipPageRecord, next corestore.RelationshipCursor) error {
	if err := s.checkOrganization(candidate.OrganizationID); err != nil {
		return err
	}
	if candidate.Kind != "relationships" || candidate.Generation <= 0 || len(records) > corestore.MaxPageSize {
		return corestore.ErrInvalidPage
	}
	var generation int64
	err := s.db.QueryRow(ctx, `SELECT generation FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$3 AND kind='relationships' FOR UPDATE`, s.organization, candidate.ProjectID, candidate.WorkItemID).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapError(ctx, err)
	}
	if generation != candidate.Generation {
		return nil
	}
	if candidate.Cleaning {
		return s.cleanRelationshipPage(ctx, candidate)
	}
	if _, err := s.trimRelationshipPages(ctx, candidate); err != nil {
		return err
	}
	types, directions, projects, items, titles, states, actors, urls, identifiers := []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}, []string{}
	numbers := []int{}
	terminals, dispatchable := []bool{}, []bool{}
	closed, started := []*time.Time{}, []*time.Time{}
	for _, record := range records {
		r := record.Relationship
		if r.OrganizationID != s.organization || r.ProjectID != candidate.ProjectID || r.WorkItemID != candidate.WorkItemID {
			return corestore.ErrTenantMismatch
		}
		types = append(types, r.Type)
		directions = append(directions, r.Direction)
		projects = append(projects, string(r.RelatedProjectID))
		items = append(items, string(r.RelatedWorkItemID))
		titles = append(titles, r.Title)
		states = append(states, r.State)
		actor := ""
		if r.ActorPrincipalID != nil {
			actor = *r.ActorPrincipalID
		}
		actors = append(actors, actor)
		urls = append(urls, record.WebURL)
		identifiers = append(identifiers, record.Identifier)
		numbers = append(numbers, r.Number)
		terminals = append(terminals, r.Terminal)
		dispatchable = append(dispatchable, r.Dispatchable)
		closed = append(closed, r.ClosedAt)
		started = append(started, r.StartedAt)
	}
	_, err = s.db.Exec(ctx, `INSERT INTO native_relationship_pages(organization_id,project_id,work_item_id,generation,relationship_type,direction,related_project_id,related_work_item_id,number,title,state,terminal,dispatchable,actor_principal_id,web_url,identifier,closed_at,started_at)
 SELECT $1,$2,$3,$4,r.* FROM unnest($5::text[],$6::text[],$7::text[],$8::text[],$9::bigint[],$10::text[],$11::text[],$12::boolean[],$13::boolean[],$14::text[],$15::text[],$16::text[],$17::timestamptz[],$18::timestamptz[]) r
 ON CONFLICT(organization_id,project_id,work_item_id,generation,relationship_type,direction,related_project_id,related_work_item_id) DO NOTHING`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Generation, types, directions, projects, items, numbers, titles, states, terminals, dispatchable, actors, urls, identifiers, closed, started)
	if err != nil {
		return mapError(ctx, err)
	}
	if candidate.Propagate && len(items) > 0 {
		_, err = s.db.Exec(ctx, `INSERT INTO native_work_item_page_dirty(organization_id,project_id,work_item_id,record_id,kind)
 SELECT DISTINCT $1,p.project,p.item,p.item,'relationships' FROM unnest($2::text[],$3::text[]) p(project,item) WHERE true
 ON CONFLICT(organization_id,project_id,work_item_id,record_id,kind) DO UPDATE SET generation=DEFAULT,source_at=clock_timestamp(),after_type='',after_direction='',after_project='',after_item='',cleaning=false`, s.organization, projects, items)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	if next.RelatedWorkItemID == "" {
		_, err = s.db.Exec(ctx, `INSERT INTO native_relationship_page_heads(organization_id,project_id,work_item_id,generation,source_at,projected_at) VALUES($1,$2,$3,$4,$5,clock_timestamp())
 ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET generation=EXCLUDED.generation,source_at=EXCLUDED.source_at,projected_at=EXCLUDED.projected_at`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Generation, candidate.SourceAt)
		if err != nil {
			return mapError(ctx, err)
		}
	}
	if next.RelatedWorkItemID == "" {
		if err := s.advanceWorkItemPageProjectionRevision(ctx); err != nil {
			return err
		}
		return s.cleanRelationshipPage(ctx, candidate)
	}
	_, err = s.db.Exec(ctx, `UPDATE native_work_item_page_dirty SET after_type=$4,after_direction=$5,after_project=$6,after_item=$7,cleaning=$8 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$3 AND kind='relationships'`, s.organization, candidate.ProjectID, candidate.WorkItemID, next.Type, next.Direction, next.RelatedProjectID, next.RelatedWorkItemID, next.RelatedWorkItemID == "")
	return mapError(ctx, err)
}

func (s *session) cleanRelationshipPage(ctx context.Context, candidate viewstore.PageCandidate) error {
	deleted, err := s.trimRelationshipPages(ctx, candidate)
	if err != nil {
		return mapError(ctx, err)
	}
	if deleted >= 200 {
		_, err = s.db.Exec(ctx, `UPDATE native_work_item_page_dirty SET cleaning=true WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$3 AND kind='relationships' AND generation=$4`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Generation)
	}
	if deleted < 200 {
		_, err = s.db.Exec(ctx, `DELETE FROM native_work_item_page_dirty WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND record_id=$3 AND kind='relationships' AND generation=$4`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Generation)
	}
	return mapError(ctx, err)
}

func (s *session) trimRelationshipPages(ctx context.Context, candidate viewstore.PageCandidate) (int64, error) {
	deleted, err := s.db.Exec(ctx, `WITH head AS MATERIALIZED (
 SELECT COALESCE((SELECT generation FROM native_relationship_page_heads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3),0) AS generation
 ),retired AS (
 (SELECT p.ctid FROM native_relationship_pages p,head h WHERE p.organization_id=$1 AND p.project_id=$2 AND p.work_item_id=$3 AND p.generation<LEAST(h.generation,$4) ORDER BY p.generation LIMIT 200)
 UNION ALL
 (SELECT p.ctid FROM native_relationship_pages p,head h WHERE p.organization_id=$1 AND p.project_id=$2 AND p.work_item_id=$3 AND p.generation>h.generation AND p.generation<$4 ORDER BY p.generation LIMIT 200)
 LIMIT 200
 ) DELETE FROM native_relationship_pages WHERE ctid IN(SELECT ctid FROM retired)`, s.organization, candidate.ProjectID, candidate.WorkItemID, candidate.Generation)
	return deleted.RowsAffected(), mapError(ctx, err)
}
