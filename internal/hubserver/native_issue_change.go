package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// A work item's change review surface (decisions sections 18.5 and 18.6).
//
// A completed item is only ever moved out of its active lane by the
// orchestrator, and the orchestrator's readiness rule was a pull request and
// nothing else: every gate kind but artifact required the issue to carry an
// open pull request. A hub-native item never has one, so a native project's
// completed item was never judged ready and the promotion was never even
// attempted -- operations.md section 8, the seventh dogfood run's "What did
// not happen: the promotion".
//
// This is what a native item is judged on instead. Two facts, and the first
// has to be stated rather than inferred from a missing pull request: a project
// with no GitHub connector can never produce one (18.6, "projects without a
// GitHub connector show the change request alone"), while a project that has a
// connector may simply not have opened the pull request yet. Those are
// different situations and a promotion decides them differently.
//
// The surface is assembled only for a caller that asks for it, because it
// costs three reads the rest of the work item resource does not need.

// readNativeIssueChange assembles one item's change review surface: the
// project's connector binding, the item's latest change request joined with
// the connector's projection when one mirrors it, and the work item revision
// the recorded change covers.
func readNativeIssueChange(ctx context.Context, query nativeQueryer, scope nativeScope, item string) (*tracker.NativeIssueChange, error) {
	binding, repositoryID, err := projectConnector(ctx, query, scope)
	if err != nil {
		return nil, err
	}
	change := &tracker.NativeIssueChange{Connector: tracker.NativeChangeConnectorNone}
	if binding != nil {
		change.Connector = tracker.NativeChangeConnectorGitHub
	}
	if change.Revision, err = readNativeChangeRevision(ctx, query, scope, item); err != nil {
		return nil, err
	}
	request, found, err := readLatestNativeChangeRequest(ctx, query, scope, item)
	if err != nil {
		return nil, err
	}
	if !found {
		return change, nil
	}
	change.ChangeID = request.ID
	// The hub's own change request is open until it merges, and 18.6 reports
	// the change request alone when nothing mirrors it.
	change.State = "open"
	version, versionFound, err := readNativeChangeVersion(ctx, query, request)
	if err != nil {
		return nil, err
	}
	if !versionFound {
		return change, nil
	}
	change.HeadSHA = version.HeadSHA
	if version.External != nil {
		change.URL = version.External.URL
	}
	if binding == nil || version.External == nil {
		return change, nil
	}
	summary, _, mirrored, err := readConnectorPullRequest(ctx, query, repositoryID, version.External.URL)
	if err != nil {
		return nil, err
	}
	if !mirrored {
		// The change names a pull request the projection has not seen yet.
		// The connector is still reported, so the promotion rule keeps
		// requiring the pull request rather than falling back.
		return change, nil
	}
	change.Number, change.Draft = summary.Number, summary.Draft
	change.State = connectorPullRequestState(summary.State)
	if summary.URL != "" {
		change.URL = summary.URL
	}
	if summary.HeadSHA != "" {
		change.HeadSHA = summary.HeadSHA
	}
	return change, nil
}

// readLatestNativeChangeRequest reads the item's most recent change request in
// the order the change endpoints already page them in.
func readLatestNativeChangeRequest(ctx context.Context, query nativeQueryer, scope nativeScope, item string) (tracker.ChangeRequest, bool, error) {
	changes, err := changeRows[tracker.ChangeRequest](ctx, query, `SELECT c.record_json FROM change_requests c
JOIN change_issue_links l ON l.change_id = c.id
WHERE c.organization_id = ? AND c.project_id = ? AND l.work_item_id = ? ORDER BY c.rowid DESC LIMIT 1`,
		scope.organization, scope.project, item)
	if err != nil {
		return tracker.ChangeRequest{}, false, err
	}
	if len(changes) == 0 {
		return tracker.ChangeRequest{}, false, nil
	}
	return changes[0], true, nil
}

// readNativeChangeVersion reads a change request's current version, which is
// where the head and the external pull request identity live.
func readNativeChangeVersion(ctx context.Context, query nativeQueryer, request tracker.ChangeRequest) (tracker.ChangeVersion, bool, error) {
	var version tracker.ChangeVersion
	if request.CurrentVersion == "" {
		return version, false, nil
	}
	var record string
	err := query.QueryRowContext(ctx, "SELECT record_json FROM change_versions WHERE id = ? AND change_id = ?",
		request.CurrentVersion, request.ID).Scan(&record)
	if errors.Is(err, sql.ErrNoRows) {
		return version, false, nil
	}
	if err != nil {
		return version, false, fmt.Errorf("read change version %s: %w", request.CurrentVersion, err)
	}
	if err := json.Unmarshal([]byte(record), &version); err != nil {
		return version, false, fmt.Errorf("decode change version %s: %w", request.CurrentVersion, err)
	}
	return version, true, nil
}

// readNativeChangeRevision reports the highest work item revision a succeeded
// attempt recorded a change for: a non-empty stored attempt diff at the
// attempt's final sequence (section 18.5, posted before the run event that
// references it, so the final diff lands before run.finished) or a change
// version the attempt published. An empty diff is a clean worktree, and an
// earlier checkpoint's diff is not what the attempt finished with when the
// final post failed, so neither is credited.
//
// The revision is the attempt's own recorded work_item_revision, which is the
// number section 9.2.1's claim clause already compares, so the two brakes
// agree on what "the item as it stands" means. Zero means no attempt has
// recorded anything for the item.
func readNativeChangeRevision(ctx context.Context, query nativeQueryer, scope nativeScope, item string) (tracker.Revision, error) {
	var revision tracker.Revision
	err := query.QueryRowContext(ctx, `SELECT COALESCE(MAX(a.work_item_revision), 0) FROM native_attempts a
WHERE a.organization_id = ? AND a.project_id = ? AND a.work_item_id = ? AND a.status = 'succeeded'
  AND (EXISTS (SELECT 1 FROM attempt_diffs d
      WHERE d.attempt_id = a.id AND d.source = 'attempt' AND d.seq = a.sequence AND d.file_count > 0)
    OR EXISTS (SELECT 1 FROM change_versions v
      JOIN change_requests c ON c.id = v.change_id
      JOIN change_issue_links l ON l.change_id = c.id
      WHERE l.work_item_id = a.work_item_id
        AND json_extract(v.record_json, '$.attempt_id') = a.id))`,
		scope.organization, scope.project, item).Scan(&revision)
	if err != nil {
		return 0, fmt.Errorf("read recorded change revision for %s: %w", item, err)
	}
	return revision, nil
}
