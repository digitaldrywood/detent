package hubserver

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func analyticsChangeColumnsPending(ctx context.Context, q nativeQueryer, scope nativeScope) (bool, error) {
	if rollup, ok := q.(analyticsRollupReader); ok {
		q = rollup.nativeQueryer
	}
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_requests c WHERE c.organization_id=? AND c.project_id=? AND c.created_at IS NULL)
 OR EXISTS (SELECT 1 FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE c.organization_id=? AND c.project_id=? AND v.head_sha IS NULL)
 OR EXISTS (SELECT 1 FROM change_evidence e JOIN change_requests c ON c.id=e.change_id WHERE c.organization_id=? AND c.project_id=? AND e.has_validator IS NULL)
 OR EXISTS (SELECT 1 FROM quality_landings l JOIN change_versions v ON v.id=l.version_id JOIN change_requests c ON c.id=v.change_id WHERE c.organization_id=? AND c.project_id=? AND l.landed_at IS NULL)
 OR EXISTS (SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='workflow.transitioned' AND from_state IS NULL)`, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project).Scan(&pending)
	return pending, err
}

func readUnconvertedAnalyticsLandings(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, quality bool) (_ []nativeAnalyticsLanding, closeErr error) {
	reopened := map[string]bool{}
	escaped := map[string]bool{}
	if quality {
		rows, err := queryChangeLegacyRows(ctx, q, `SELECT work_item_id,data_json,from_state FROM collaboration_events WHERE organization_id=? AND project_id=? AND type='workflow.transitioned' AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?) AND recorded_at>=? AND recorded_at<? ORDER BY sequence`, scope.organization, scope.project, formatHubTime(w.From), formatHubTime(w.To), w.From.UTC().Add(-24*time.Hour).Format(time.DateOnly), w.To.UTC().Add(48*time.Hour).Format(time.DateOnly))
		if err != nil {
			return nil, err
		}
		defer func() { closeErr = errors.Join(closeErr, rows.Close()) }()
		for rows.Next() {
			var item, raw string
			var from sql.NullString
			if err := rows.Scan(&item, &raw, &from); err != nil {

				return nil, err
			}
			if !from.Valid {
				var data tracker.CollaborationData
				if err := decodeChangeColumnsJSON(ctx, []byte(raw), &data); err != nil {

					return nil, err
				}
				from.String = data.FromState
			}
			if strings.EqualFold(from.String, "done") {
				reopened[item] = true
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
		discussions, err := queryChangeLegacyRows(ctx, q, `SELECT e.record_json FROM change_evidence e JOIN change_requests c ON c.id=e.change_id WHERE c.organization_id=? AND c.project_id=? AND e.kind='discussion' AND ((e.escape_occurrence_id<>'' AND e.escape_observed_at>=? AND e.escape_observed_at<?) OR e.has_validator IS NULL) ORDER BY e.sequence`, scope.organization, scope.project, changeColumnTime(w.From), changeColumnTime(w.To))
		if err != nil {
			return nil, err
		}
		defer func() { closeErr = errors.Join(closeErr, discussions.Close()) }()
		for discussions.Next() {
			var raw string
			if err := discussions.Scan(&raw); err != nil {
				return nil, err
			}
			var discussion tracker.ChangeDiscussion
			if err := decodeChangeColumnsJSON(ctx, []byte(raw), &discussion); err != nil {
				return nil, err
			}
			if e := discussion.Escape; e != nil && !e.ObservedAt.Before(w.From) && e.ObservedAt.Before(w.To) {
				escaped[discussion.VersionID] = true
			}
		}
		if err := errors.Join(discussions.Err(), discussions.Close()); err != nil {
			return nil, err
		}
	}
	issues, issueArgs := reportsIssueClause(ctx, "c.work_item_id")
	rows, err := queryChangeLegacyRows(ctx, q, `SELECT c.id,c.work_item_id,c.record_json,v.id,v.record_json,COALESCE(l.record_json,'') FROM change_requests c JOIN change_versions v ON v.change_id=c.id LEFT JOIN quality_landings l ON l.version_id=v.id WHERE c.organization_id=? AND c.project_id=?`+issues+` ORDER BY c.id,v.number`, append([]any{scope.organization, scope.project}, issueArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { closeErr = errors.Join(closeErr, rows.Close()) }()
	name := "landings"
	if quality {
		name = "quality_landings"
	}
	population := newAnalyticsOrderedPopulation(ctx, name, func(a, b nativeAnalyticsLanding) int {
		if order := a.Landing.LandedAt.Compare(b.Landing.LandedAt); order != 0 {
			return order
		}
		if order := cmp.Compare(a.ChangeID, b.ChangeID); order != 0 {
			return order
		}
		return cmp.Compare(a.Landing.VersionID, b.Landing.VersionID)
	})
	for rows.Next() {
		var value nativeAnalyticsLanding
		var changeRaw, versionID, versionRaw, qualityRaw string
		if err := rows.Scan(&value.ChangeID, &value.WorkItemID, &changeRaw, &versionID, &versionRaw, &qualityRaw); err != nil {
			return nil, err
		}
		var change tracker.ChangeRequest
		var version tracker.ChangeVersion
		if err := decodeChangeColumnsJSON(ctx, []byte(changeRaw), &change); err != nil {
			return nil, err
		}
		if err := decodeChangeColumnsJSON(ctx, []byte(versionRaw), &version); err != nil {
			return nil, err
		}
		if quality && qualityRaw != "" {
			if err := decodeQualityLanding(ctx, qualityRaw, &value.Landing); err != nil {
				return nil, err
			}
		} else {
			landing := change.Landed
			if landing == nil || landing.VersionID != versionID || landing.HeadSHA != version.HeadSHA || len(landing.MergeSHA) != 40 && len(landing.MergeSHA) != 64 {
				continue
			}
			value.Landing = *landing
		}
		if !value.Landing.LandedAt.Before(w.To) || value.Landing.LandedAt.Before(w.From) && (!quality || !reopened[string(value.WorkItemID)] && !escaped[value.Landing.VersionID]) {
			continue
		}
		if !quality {
			value.Landing.Quality = nil
		}
		population.add(value)
	}
	return population.page(ctx, name), rows.Err()
}

func legacyQualityEvidenceMatches(ctx context.Context, q nativeQueryer, kind, raw string, w operatortool.AnalyticsWindow) (bool, error) {
	if kind == "discussion" {
		var discussion tracker.ChangeDiscussion
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &discussion); err != nil {
			return false, err
		}
		e := discussion.Escape
		return e != nil && !e.ObservedAt.Before(w.From) && e.ObservedAt.Before(w.To), nil
	}
	var review tracker.ChangeReview
	if err := decodeChangeColumnsJSON(ctx, []byte(raw), &review); err != nil {
		return false, err
	}
	if review.Decision != "changes_requested" || review.CreatedAt.Before(w.From) || !review.CreatedAt.Before(w.To) {
		return false, nil
	}
	versions, err := changeColumnRecords[tracker.ChangeVersion](ctx, q, `SELECT newer.record_json FROM change_versions current JOIN change_versions newer ON newer.change_id=current.change_id AND newer.number>current.number WHERE current.id=?`, review.VersionID)
	if err != nil {
		return false, err
	}
	for _, version := range versions {
		if !version.CreatedAt.After(review.CreatedAt) {
			return false, nil
		}
	}
	return true, nil
}

type legacyAnalyticsRows struct{ *changeLegacyRows }

func (r *legacyAnalyticsRows) Close() { r.err = errors.Join(r.err, r.changeLegacyRows.Close()) }
func queryLegacyAnalyticsRows(ctx context.Context, q nativeQueryer, statement string, args ...any) (*legacyAnalyticsRows, error) {
	rows, err := queryChangeLegacyRows(ctx, q, statement, args...)
	if err != nil {
		return nil, err
	}
	return &legacyAnalyticsRows{rows}, nil
}
