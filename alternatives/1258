package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readLegacyChangeAttempts(ctx context.Context, query nativeQueryer, scope nativeScope, item string) ([]any, error) {
	versions, err := changeColumnRecords[tracker.ChangeVersion](ctx, query, `SELECT COALESCE(NULLIF(v.payload_object_key,''),v.record_json) FROM change_versions v INDEXED BY change_versions_change_attempt_unknown JOIN change_requests c ON c.id=v.change_id JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? AND v.attempt_id IS NULL ORDER BY v.rowid`, scope.organization, scope.project, item)
	if err != nil {
		return nil, err
	}
	ids := []any{}
	for _, version := range versions {
		if version.AttemptID != "" {
			ids = append(ids, version.AttemptID)
		}
	}
	return ids, nil
}

func changeArtifactAttemptMatches(ctx context.Context, query nativeQueryer, scope nativeScope, version, attempt string) (bool, error) {
	var stored sql.NullString
	var raw string
	err := query.QueryRowContext(ctx, `SELECT v.attempt_id,COALESCE(NULLIF(v.payload_object_key,''),v.record_json) FROM change_versions v JOIN change_requests c ON c.id=v.change_id WHERE v.id=? AND c.organization_id=? AND c.project_id=?`, version, scope.organization, scope.project).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored.Valid {
		return stored.String == attempt, nil
	}
	var value tracker.ChangeVersion
	if err := decodeChangeColumnsJSON(ctx, []byte(raw), &value); err != nil {
		return false, err
	}
	return value.AttemptID == attempt, nil
}

func readRecoveryCommentIDs(ctx context.Context, query nativeQueryer, scope nativeScope, item, order string, bounded bool) (ids []string, err error) {
	var unknown bool
	if err := query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? AND (change_actor_kind IS NULL OR change_edited_by_kind IS NULL))`, scope.organization, scope.project, item).Scan(&unknown); err != nil {
		return nil, err
	}
	if !unknown {
		return nativePageIDs(ctx, query, `SELECT id FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? AND (change_actor_kind='human' OR change_edited_by_kind='human' OR id=(SELECT id FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? AND change_actor_kind<>'human' ORDER BY sequence DESC LIMIT 1))`+order, scope.organization, scope.project, item, scope.organization, scope.project, item)
	}
	rows, err := queryChangeLegacyRows(ctx, query, `SELECT id,actor_json,edited_by_json FROM native_comments WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY sequence DESC`, scope.organization, scope.project, item)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	ids = []string{}
	latestMachine := false
	for rows.Next() {
		var id, raw string
		var edited sql.NullString
		if err := rows.Scan(&id, &raw, &edited); err != nil {
			return nil, err
		}
		var actor, editor tracker.Actor
		if err := decodeStorageJSON(ctx, []byte(raw), &actor); err != nil {
			return nil, err
		}
		if edited.Valid {
			if err := decodeStorageJSON(ctx, []byte(edited.String), &editor); err != nil {
				return nil, err
			}
		}
		keep := actor.Kind == "human" || editor.Kind == "human" || !latestMachine
		if actor.Kind != "human" {
			latestMachine = true
		}
		if keep {
			ids = append(ids, id)
		}
		if bounded && len(ids) == 33 {
			break
		}
	}
	if !bounded {
		slices.Reverse(ids)
	}
	return ids, rows.Err()
}

func readLegacyIssueChangeSideData(ctx context.Context, query nativeQueryer, scope nativeScope, item string, issue *tracker.NativeIssue) (err error) {
	var id, raw string
	var current, landed, base, merge sql.NullString
	err = query.QueryRowContext(ctx, `SELECT c.id,c.record_json,c.claim_version_id,c.landed_version_id,c.landed_base_ref,c.landed_merge_sha FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? ORDER BY c.rowid DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&id, &raw, &current, &landed, &base, &merge)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var change tracker.ChangeRequest
		if !current.Valid || !landed.Valid || !base.Valid || !merge.Valid {
			if err := decodeChangeColumnsJSON(ctx, []byte(raw), &change); err != nil {
				return err
			}
			if !current.Valid {
				current.String = change.CurrentVersion
			}
			if change.Landed != nil {
				if !landed.Valid {
					landed.String = change.Landed.VersionID
				}
				if !base.Valid {
					base.String = change.Landed.BaseRef
				}
				if !merge.Valid {
					merge.String = change.Landed.MergeSHA
				}
			}
		}
		projection := &tracker.NativeListChange{}
		if issue.ListChange != nil {
			projection.LandingPending = issue.ListChange.LandingPending
		}
		if current.String != "" && landed.String == current.String {
			projection.Branch, projection.HeadSHA = base.String, merge.String
		} else if current.String != "" {
			var head sql.NullString
			var versionRaw string
			err := query.QueryRowContext(ctx, `SELECT head_sha,COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND id=?`, id, current.String).Scan(&head, &versionRaw)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil {
				if head.Valid {
					projection.HeadSHA = head.String
				} else {
					var version tracker.ChangeVersion
					if err := decodeChangeColumnsJSON(ctx, []byte(versionRaw), &version); err != nil {
						return err
					}
					projection.HeadSHA = version.HeadSHA
				}
			}
		}
		issue.ListChange = projection
	}
	rows, err := queryChangeLegacyRows(ctx, query, `SELECT v.external_provider,v.external_id,v.external_url,COALESCE(NULLIF(v.payload_object_key,''),v.record_json) FROM change_versions v JOIN change_requests c ON c.id=v.change_id JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? AND ((v.external_provider='github' AND v.external_url<>'') OR v.external_provider IS NULL OR v.external_url IS NULL) ORDER BY c.rowid DESC,v.number DESC`, scope.organization, scope.project, item)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	issue.PullRequest = nil
	for rows.Next() {
		var provider, externalID, url sql.NullString
		var raw string
		if err := rows.Scan(&provider, &externalID, &url, &raw); err != nil {
			return err
		}
		if !provider.Valid || !externalID.Valid || !url.Valid {
			var version tracker.ChangeVersion
			if err := decodeChangeColumnsJSON(ctx, []byte(raw), &version); err != nil {
				return err
			}
			if version.External != nil {
				if !provider.Valid {
					provider.String = version.External.Provider
				}
				if !externalID.Valid {
					externalID.String = version.External.ID
				}
				if !url.Valid {
					url.String = version.External.URL
				}
			}
		}
		if provider.String != "github" || url.String == "" {
			continue
		}
		reference := tracker.ChangeExternalReference{Provider: provider.String, ID: externalID.String, URL: url.String}
		if number := externalPullRequestNumber(reference); number > 0 {
			issue.PullRequest = &tracker.NativePullRequest{Number: number, URL: url.String}
		}
		return nil
	}
	return rows.Err()
}

func readNativePolicyLanding(ctx context.Context, query policyQuerier, organization, project string, source *policy.RepositorySource) (policyLanding, bool, error) {
	var after string
	for {
		var candidate struct {
			id, raw              string
			version, merge, base sql.NullString
		}
		err := query.QueryRowContext(ctx, `SELECT id,record_json,landed_version_id,landed_merge_sha,landed_base_ref FROM (
 SELECT * FROM (SELECT c.id,c.record_json,c.landed_version_id,c.landed_merge_sha,c.landed_base_ref FROM change_requests c WHERE c.organization_id=? AND c.project_id=? AND c.id>? AND c.landed_merge_sha=? AND c.landed_base_ref=? ORDER BY c.id LIMIT 1)
 UNION ALL SELECT * FROM (SELECT c.id,c.record_json,c.landed_version_id,c.landed_merge_sha,c.landed_base_ref FROM change_requests c WHERE c.organization_id=? AND c.project_id=? AND c.id>? AND c.created_at IS NULL ORDER BY c.id LIMIT 1)
) ORDER BY id LIMIT 1`, organization, project, after, source.Commit, source.DefaultBranch, organization, project, after).Scan(&candidate.id, &candidate.raw, &candidate.version, &candidate.merge, &candidate.base)
		if errors.Is(err, sql.ErrNoRows) {
			return policyLanding{}, false, nil
		}
		if err != nil {
			return policyLanding{}, false, err
		}
		after = candidate.id
		var change tracker.ChangeRequest
		if err := decodeChangeColumnsJSON(ctx, []byte(candidate.raw), &change); err != nil {
			return policyLanding{}, false, err
		}
		if change.Landed == nil {
			continue
		}
		merge, base := candidate.merge.String, candidate.base.String
		if !candidate.merge.Valid {
			merge = change.Landed.MergeSHA
		}
		if !candidate.base.Valid {
			base = change.Landed.BaseRef
		}
		if merge != source.Commit || base != source.DefaultBranch {
			continue
		}
		if !candidate.version.Valid {
			candidate.version.String = change.Landed.VersionID
		}
		var repository sql.NullString
		var raw string
		err = query.QueryRowContext(ctx, `SELECT repository,COALESCE(NULLIF(payload_object_key,''),record_json) FROM change_versions WHERE change_id=? AND id=?`, change.ID, candidate.version.String).Scan(&repository, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return policyLanding{}, false, err
		}
		if !repository.Valid {
			var version tracker.ChangeVersion
			if err := decodeChangeColumnsJSON(ctx, []byte(raw), &version); err != nil {
				return policyLanding{}, false, err
			}
			repository.String = version.Repository
		}
		if strings.EqualFold(validationRepository(repository.String), source.Repository) {
			return policyLanding{ChangeID: change.ID, Actor: change.Landed.Actor.PrincipalID}, true, nil
		}
		return policyLanding{}, false, nil
	}
}

func latestRecoveryLandingAttempt(ctx context.Context, query nativeQueryer, scope nativeScope, item string) (id string, err error) {
	if attemptSummaryRead(ctx) {
		err := query.QueryRowContext(ctx, `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND has_landing_receipt=1 ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return id, err
	}
	rows, err := queryChangeLegacyRows(ctx, query, `SELECT id,has_landing_receipt,COALESCE(NULLIF(payload_object_key,''),data_json) FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND (has_landing_receipt=1 OR has_landing_receipt IS NULL) ORDER BY fencing_token DESC`, scope.organization, scope.project, item)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var id, raw string
		var present sql.NullBool
		if err := rows.Scan(&id, &present, &raw); err != nil {
			return "", err
		}
		if present.Valid {
			return id, nil
		}
		var run tracker.NativeRunData
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &run); err != nil {
			return "", err
		}
		if run.Runtime != nil && run.Runtime.Landing != nil {
			return id, nil
		}
	}
	return "", rows.Err()
}

func readEscapeOccurrence(ctx context.Context, query nativeQueryer, change, version, occurrence string) (string, error) {
	var before int64
	for {
		statement := `SELECT sequence,COALESCE(NULLIF(payload_object_key,''),record_json),escape_occurrence_id FROM change_evidence WHERE change_id=? AND version_id=? AND kind='discussion' AND (escape_occurrence_id=? OR escape_occurrence_id IS NULL)`
		args := []any{change, version, occurrence}
		if before != 0 {
			statement += ` AND sequence<?`
			args = append(args, before)
		}
		type record struct {
			raw   string
			known sql.NullString
		}
		records, err := readChangeLegacyPage(ctx, query, statement+` ORDER BY sequence DESC LIMIT ?`, func(rows *sql.Rows) (record, error) {
			var value record
			err := rows.Scan(&before, &value.raw, &value.known)
			return value, err
		}, append(args, changeLegacyReadLimit)...)
		if err != nil {
			return "", err
		}
		for _, value := range records {
			if value.known.Valid {
				return value.raw, nil
			}
			var discussion tracker.ChangeDiscussion
			if err := decodeChangeColumnsJSON(ctx, []byte(value.raw), &discussion); err != nil {
				return "", err
			}
			if discussion.Escape != nil && discussion.Escape.OccurrenceID == occurrence {
				return marshalNative(discussion)
			}
		}
		if len(records) < changeLegacyReadLimit {
			return "", sql.ErrNoRows
		}
	}
}

func qualityReopenOccurrence(ctx context.Context, query nativeQueryer, scope nativeScope, item string, escape tracker.QualityEscape) (bool, error) {
	var from, to sql.NullString
	var raw, at string
	err := query.QueryRowContext(ctx, `SELECT from_state,to_state,data_json,recorded_at FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND id=? AND type='workflow.transitioned'`, scope.organization, scope.project, item, escape.OccurrenceID).Scan(&from, &to, &raw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !from.Valid || !to.Valid {
		var data tracker.CollaborationData
		if err := decodeChangeColumnsJSON(ctx, []byte(raw), &data); err != nil {
			return false, err
		}
		from.String, to.String = data.FromState, data.ToState
	}
	observed, err := parseTimeValue(at)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(from.String, "done") || !observed.Equal(escape.ObservedAt) {
		return false, nil
	}
	var valid bool
	err = query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_states WHERE project_id=? AND detent_state=? AND terminal=0)`, scope.project, to.String).Scan(&valid)
	return valid, err
}
