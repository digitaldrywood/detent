package hubserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readLegacyIssueChangeSideData(ctx context.Context, query hubapi.NativeQueryer, scope hubscope.NativeScope, item string, issue *tracker.NativeIssue) (err error) {
	var id, raw string
	var current, landed, base, merge sql.NullString
	err = query.QueryRowContext(ctx, `SELECT c.id,c.record_json,c.claim_version_id,c.landed_version_id,c.landed_base_ref,c.landed_merge_sha FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? ORDER BY c.rowid DESC LIMIT 1`, scope.Organization, scope.Project, item).Scan(&id, &raw, &current, &landed, &base, &merge)
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
	rows, err := queryChangeLegacyRows(ctx, query, `SELECT v.external_provider,v.external_id,v.external_url,COALESCE(NULLIF(v.payload_object_key,''),v.record_json) FROM change_versions v JOIN change_requests c ON c.id=v.change_id JOIN change_issue_links l ON l.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? AND ((v.external_provider='github' AND v.external_url<>'') OR v.external_provider IS NULL OR v.external_url IS NULL) ORDER BY c.rowid DESC,v.number DESC`, scope.Organization, scope.Project, item)
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
