package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ChangeRecoveryProjectionBatch struct {
	organization tracker.OrganizationID
	reviews      []eventstore.Evidence
	pushes       []eventstore.Event
}

func (r *Repository) BackfillChangeRecoveryProjections(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int) (int, error) {
	batch, err := r.PrepareChangeRecoveryProjections(ctx, organization, content, limit)
	if err != nil {
		return 0, err
	}
	return r.ApplyChangeRecoveryProjections(ctx, batch)
}

func (r *Repository) PrepareChangeRecoveryProjections(ctx context.Context, organization tracker.OrganizationID, content *eventstore.Content, limit int) (*ChangeRecoveryProjectionBatch, error) {
	if content == nil || organization == "" || limit < 1 || limit > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	var reviews []pgquery.ChangeEvidence
	var pushes []pgquery.CollaborationEvent
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		var err error
		reviews, err = s.queries.RecordUnprojectedChangeReviews(ctx, pgquery.RecordUnprojectedChangeReviewsParams{OrganizationID: organization, Limit: int32(limit)})
		if err != nil || len(reviews) == limit {
			return err
		}
		pushes, err = s.queries.RecordUnprojectedChangePushes(ctx, pgquery.RecordUnprojectedChangePushesParams{OrganizationID: organization, Limit: int32(limit - len(reviews))})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read change projections: %w", err)
	}
	batch := &ChangeRecoveryProjectionBatch{organization: organization}
	for _, stored := range reviews {
		row := evidenceRecord(stored)
		raw, err := content.Evidence(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("read review projection object: %w", err)
		}
		value, err := eventstore.DecodeChangeReview(row, raw)
		if err != nil {
			return nil, err
		}
		projected, err := eventstore.ProjectChangeReview(row, value)
		if err != nil {
			return nil, err
		}
		batch.reviews = append(batch.reviews, projected)
	}
	for _, stored := range pushes {
		row := eventRecord(stored)
		value, err := content.Event(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("read push projection object: %w", err)
		}
		if int64(value.SchemaVersion) != row.SchemaVersion {
			return nil, eventstore.ErrInvalidRecord
		}
		if value.Data.Run == nil {
			if row.RunID != "" || row.AttemptID != "" {
				return nil, eventstore.ErrInvalidRecord
			}
		} else if value.Data.Run.RunID != row.RunID || value.Data.Run.AttemptID != row.AttemptID {
			return nil, eventstore.ErrInvalidRecord
		}
		batch.pushes = append(batch.pushes, eventstore.ProjectChangePush(row, value))
	}
	return batch, nil
}

func (r *Repository) ApplyChangeRecoveryProjections(ctx context.Context, batch *ChangeRecoveryProjectionBatch) (int, error) {
	if batch == nil || batch.organization == "" || len(batch.reviews)+len(batch.pushes) > corestore.MaxPageSize {
		return 0, corestore.ErrInvalidPage
	}
	count := len(batch.reviews) + len(batch.pushes)
	if count == 0 {
		return 0, nil
	}
	type update struct {
		query    string
		args     []any
		expected int
	}
	var updates []update
	for _, review := range []bool{true, false} {
		var values []string
		var args []any
		add := func(types []string, fields ...any) {
			placeholders := make([]string, len(fields))
			for i, field := range fields {
				args = append(args, field)
				placeholders[i] = fmt.Sprintf("$%d::%s", len(args), types[i])
			}
			values = append(values, "("+strings.Join(placeholders, ",")+")")
		}
		var query string
		expected := len(batch.pushes)
		if review {
			expected = len(batch.reviews)
			for _, row := range batch.reviews {
				add([]string{"text", "text", "bigint", "text", "text", "text", "text", "text", "text", "timestamptz", "text", "text", "bigint", "bigint", "bigint", "text", "boolean", "boolean", "bigint", "text"}, row.OrganizationID, row.ChangeID, row.Sequence, row.VersionID, row.Kind, row.SourceKey, row.ActorKind, row.ActorPrincipalID, row.Decision, row.CreatedAt, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.PayloadFormatVersion, row.ReviewKind, row.ReviewLanding, row.ReviewSessionKnown, row.ReviewSessionID, row.ReviewSessionMachine)
			}
			query = `UPDATE change_evidence r SET review_kind=v.review_kind,review_landing=v.review_landing,review_session_known=v.review_session_known,review_session_id=v.review_session_id,review_session_machine=v.review_session_machine FROM (VALUES ` + strings.Join(values, ",") + `) AS v(organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version,review_kind,review_landing,review_session_known,review_session_id,review_session_machine)
WHERE r.organization_id=v.organization_id AND r.change_id=v.change_id AND r.sequence=v.sequence AND (r.review_kind='' OR NOT r.review_session_known) AND (r.review_kind='' OR (r.review_kind=v.review_kind AND r.review_landing=v.review_landing)) AND ROW(r.organization_id,r.change_id,r.sequence,r.version_id,r.kind,r.source_key,r.actor_kind,r.actor_principal_id,r.decision,r.created_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,r.payload_content_bytes,r.payload_format_version) IS NOT DISTINCT FROM ROW(v.organization_id,v.change_id,v.sequence,v.version_id,v.kind,v.source_key,v.actor_kind,v.actor_principal_id,v.decision,v.created_at,v.payload_object_key,v.payload_sha256,v.payload_bytes,v.payload_content_bytes,v.payload_format_version)`
		} else {
			for _, row := range batch.pushes {
				add([]string{"text", "text", "text", "bigint", "text", "text", "bigint", "text", "text", "text", "text", "timestamptz", "text", "text", "bigint", "bigint", "bigint", "bigint", "bigint", "text", "text", "text", "text", "text", "boolean", "text", "text", "text", "text", "text", "text", "text", "text"}, row.OrganizationID, row.ProjectID, row.WorkItemID, row.Sequence, row.ID, row.Type, row.SchemaVersion, row.ActorKind, row.ActorPrincipalID, row.RunID, row.AttemptID, row.RecordedAt, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.PayloadFormatVersion, row.ProjectSequence, row.EventRevision, row.ChangeID, row.VersionID, row.HeadSHA, row.FromState, row.ToState, row.LandingWaiting, row.LandingChangeID, row.LandingVersionID, row.LandingHeadSHA, row.PushChangeID, row.PushVersionID, row.PushHeadSHA, row.PushExternalEffect, row.PushEffectState)
			}
			query = `UPDATE collaboration_events r SET push_known=true,push_change_id=v.push_change_id,push_version_id=v.push_version_id,push_head_sha=v.push_head_sha,push_external_effect=v.push_external_effect,push_effect_state=v.push_effect_state FROM (VALUES ` + strings.Join(values, ",") + `) AS v(organization_id,project_id,work_item_id,sequence,id,type,schema_version,actor_kind,actor_principal_id,run_id,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version,project_sequence,event_revision,change_id,version_id,head_sha,from_state,to_state,landing_waiting,landing_change_id,landing_version_id,landing_head_sha,push_change_id,push_version_id,push_head_sha,push_external_effect,push_effect_state)
WHERE r.organization_id=v.organization_id AND r.id=v.id AND NOT r.push_known AND ROW(r.organization_id,r.project_id,r.work_item_id,r.sequence,r.id,r.type,r.schema_version,r.actor_kind,r.actor_principal_id,r.run_id,r.attempt_id,r.recorded_at,r.payload_object_key,r.payload_sha256,r.payload_bytes,r.payload_content_bytes,r.payload_format_version,r.project_sequence,r.event_revision,r.change_id,r.version_id,r.head_sha,r.from_state,r.to_state,r.landing_waiting,r.landing_change_id,r.landing_version_id,r.landing_head_sha) IS NOT DISTINCT FROM ROW(v.organization_id,v.project_id,v.work_item_id,v.sequence,v.id,v.type,v.schema_version,v.actor_kind,v.actor_principal_id,v.run_id,v.attempt_id,v.recorded_at,v.payload_object_key,v.payload_sha256,v.payload_bytes,v.payload_content_bytes,v.payload_format_version,v.project_sequence,v.event_revision,v.change_id,v.version_id,v.head_sha,v.from_state,v.to_state,v.landing_waiting,v.landing_change_id,v.landing_version_id,v.landing_head_sha)`
		}
		if expected == 0 {
			continue
		}
		updates = append(updates, update{query: query, args: args, expected: expected})
	}
	err := r.transaction(ctx, batch.organization, corestore.Background, false, func(s *session) error {
		for _, update := range updates {
			result, err := s.db.Exec(ctx, update.query, update.args...)
			if err != nil {
				return err
			}
			changed := result.RowsAffected()
			if changed != int64(update.expected) {
				return corestore.ErrRevisionConflict
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("apply change projections: %w", err)
	}
	return count, nil
}
