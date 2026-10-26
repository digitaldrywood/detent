package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrReleasePickMismatch = errors.New("release pick provenance mismatch")

type ReleasePickVerifier interface {
	VerifyReleasePick(context.Context, tracker.ReleasePickInput) error
	VerifyReleasePickRefresh(context.Context, tracker.ReleasePickInput, tracker.ReleasePickInput) error
}

var releaseBranchPattern = regexp.MustCompile(`^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func readReleaseDelivery(ctx context.Context, query nativeQueryer, scope nativeScope, item string) (tracker.ReleaseDelivery, bool, error) {
	var d tracker.ReleaseDelivery
	var at string
	var version sql.NullString
	err := query.QueryRowContext(ctx, `SELECT source_issue_id, source_change_id, source_merge_sha, repository, pr_number,
head_ref, head_sha, base_ref, base_sha, frozen_sha, change_id, version_id, principal_id, actor_kind, via_key, associated_at
FROM release_deliveries WHERE organization_id = ? AND project_id = ? AND work_item_id = ?`, scope.organization, scope.project, item).Scan(
		&d.SourceIssueID, &d.SourceChangeID, &d.SourceMergeSHA, &d.Repository, &d.PRNumber, &d.HeadRef, &d.HeadSHA, &d.BaseRef, &d.BaseSHA, &d.FrozenSHA,
		&d.ChangeID, &version, &d.Actor.PrincipalID, &d.Actor.Kind, &d.Actor.ViaKey, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.VersionID = version.String
	d.AssociatedAt, err = parseTimeValue(at)
	return d, err == nil, err
}

func validateReleasePick(r tracker.AssociateReleasePick) error {
	if r.PRNumber < 1 {
		return nativePublicInvalid("Release association requires an existing PR number")
	}
	return validatePreparedReleasePick(r.ReleasePickInput, r.ExpectedRevision)
}

func validatePreparedReleasePick(r tracker.ReleasePickInput, revision tracker.Revision) error {
	if r.HeadSHA == r.BaseSHA || r.HeadRef == r.BaseRef || r.SourceMergeSHA == r.FrozenSHA {
		return nativePublicInvalid("Release pick must identify a distinct source fix and PR head")
	}
	if !validNativeID(string(r.SourceIssueID), "wi") || !validNativeID(r.SourceChangeID, "change") || revision < 1 || !releaseBranchPattern.MatchString(r.BaseRef) || !validReleaseHeadRef(r.HeadRef) {
		return nativePublicInvalid("Release delivery requires a source issue and Change, revision, pick branch and frozen release branch")
	}
	for _, sha := range []string{r.SourceMergeSHA, r.HeadSHA, r.BaseSHA, r.FrozenSHA} {
		if !validCommitID(sha) {
			return nativePublicInvalid("Release delivery requires exact commit identities")
		}
	}
	return nil
}

func validReleaseHeadRef(ref string) bool {
	return ref != "" && len(ref) <= 256 && !strings.HasPrefix(ref, "-") && !strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock") && !strings.Contains(ref, "..") && !strings.Contains(ref, "@{") && !strings.ContainsAny(ref, " ~^:?*[\\\t\r\n")
}

func sameReleasePickDelivery(prior, next tracker.ReleasePickInput) bool {
	next.BaseSHA, next.HeadSHA = prior.BaseSHA, prior.HeadSHA
	return prior == next
}

func (s *Service) prepareReleasePick(ctx context.Context, scope nativeScope, item string, r tracker.AssociateReleasePick) error {
	if scope.credential.Scope != apiScopeOperator && scope.credential.Scope != apiScopeAdmin || scope.credential.Runner.RunnerID != "" {
		return nativeNotFound()
	}
	if err := validateReleasePick(r); err != nil {
		return err
	}
	connector, _, _, err := projectRepositoryBinding(ctx, s.database.reader, scope)
	if err != nil {
		return err
	}
	if connector == nil || r.Repository != "https://github.com/"+connector.Repository {
		return nativePublicInvalid("Release PR repository must match the project's configured repository")
	}
	existing, found, err := readReleaseDelivery(ctx, s.database.reader, scope, item)
	if err != nil {
		return err
	}
	if found && existing.ReleasePickInput == r.ReleasePickInput {
		return nil
	}
	verifier, ok := s.config.ReconcileBackend.(ReleasePickVerifier)
	if !ok {
		return errors.New("repository transport does not support release provenance verification")
	}
	verify := func() error { return verifier.VerifyReleasePick(ctx, r.ReleasePickInput) }
	if found {
		verify = func() error {
			return verifier.VerifyReleasePickRefresh(ctx, existing.ReleasePickInput, r.ReleasePickInput)
		}
	}
	if err := verify(); err != nil {
		if errors.Is(err, ErrReleasePickMismatch) {
			return nativePublicInvalid("Release PR provenance verification failed")
		}
		return fmt.Errorf("verify release PR: %w", err)
	}
	return nil
}

func (s *Service) associateReleasePick(c echo.Context) error {
	var r tracker.AssociateReleasePick
	if err := decodeAPIJSON(c, &r); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.prepareReleasePick(c.Request().Context(), nativeRequestScope(c), c.Param("item"), r); err != nil {
		return s.nativeAPIError(c, err)
	}
	err := s.nativeMutation(c, r.Mutation, r, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.associateReleasePickCommand(ctx, tx, scope, c.Param("item"), r, now)
	})
	s.pullRequests.invalidate(pullRequestCacheKey(nativeRequestScope(c), c.Param("item")))
	return err
}

func (s *Service) associateReleasePickCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, r tracker.AssociateReleasePick, now time.Time) (any, error) {
	issue, _, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return nil, err
	}
	existing, found, err := readReleaseDelivery(ctx, tx, scope, item)
	if err != nil {
		return nil, err
	}
	if found {
		if existing.ReleasePickInput == r.ReleasePickInput {
			return existing, nil
		}
	}
	if err := s.checkReleasePickDelivery(ctx, tx, scope, item, r, now, found); err != nil {
		return nil, err
	}
	if found {
		change, err := readChange(ctx, tx, scope, item, existing.ChangeID)
		if err != nil {
			return nil, err
		}
		d := tracker.ReleaseDelivery{ReleasePickInput: r.ReleasePickInput, ChangeID: change.ID, Actor: scope.actor(), AssociatedAt: now}
		if _, err := tx.ExecContext(ctx, `UPDATE release_deliveries SET head_sha = ?, base_sha = ?, version_id = NULL, principal_id = ?, actor_kind = ?, via_key = ?, associated_at = ? WHERE work_item_id = ?`, d.HeadSHA, d.BaseSHA, d.Actor.PrincipalID, d.Actor.Kind, d.Actor.ViaKey, formatHubTime(now), item); err != nil {
			return nil, err
		}
		if _, err := persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: d.ChangeID, HeadSHA: d.HeadSHA}}, now); err != nil {
			return nil, err
		}
		if err := routeChangeForRework(ctx, tx, scope, issue, now); err != nil {
			return nil, err
		}
		change.CurrentVersion, change.UpdatedAt = "", now
		change.Revision++
		raw, err := marshalNative(change)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE change_requests SET record_json = ?, claim_version_id = '', claim_head_sha = '' WHERE id = ?`, raw, change.ID); err != nil {
			return nil, err
		}
		return d, nil
	}
	_, repositoryID, _, err := projectRepositoryBinding(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	approved, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return nil, err
	}
	if err := approveReleaseDeliveryContract(&issue, approved.Policy, now); err != nil {
		return nil, err
	}
	created, err := s.createChangeCommand(ctx, tx, scope, item, tracker.CreateChange{Title: issue.Title}, now)
	if err != nil {
		return nil, err
	}
	change, ok := created.(tracker.ChangeRequest)
	if !ok {
		return nil, errors.New("create release Change returned an invalid receipt")
	}
	d := tracker.ReleaseDelivery{ReleasePickInput: r.ReleasePickInput, ChangeID: change.ID, Actor: scope.actor(), AssociatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO release_deliveries (work_item_id, organization_id, project_id, source_issue_id, source_change_id, source_merge_sha,
repository_id, repository, pr_number, head_ref, head_sha, base_ref, base_sha, frozen_sha, change_id, version_id, principal_id, actor_kind, via_key, associated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item, scope.organization, scope.project, d.SourceIssueID, d.SourceChangeID, d.SourceMergeSHA, sql.NullInt64{Int64: repositoryID, Valid: repositoryID != 0},
		d.Repository, d.PRNumber, d.HeadRef, d.HeadSHA, d.BaseRef, d.BaseSHA, d.FrozenSHA, d.ChangeID, nil, d.Actor.PrincipalID, d.Actor.Kind, d.Actor.ViaKey, formatHubTime(now))
	if err != nil {
		return nil, err
	}
	_, err = persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: d.ChangeID, VersionID: d.VersionID, HeadSHA: d.HeadSHA}}, now)
	return d, err
}

func (s *Service) checkReleasePickDelivery(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, r tracker.AssociateReleasePick, now time.Time, publishing bool) error {
	issue, issueID, err := readNativeIssue(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	if err := requireNativeEdit(issue, r.ExpectedRevision); err != nil {
		return err
	}
	existing, refreshing, err := readReleaseDelivery(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	if publishing && r.PRNumber > 0 && !refreshing {
		return nativeInvalid("Existing PR publication requires an associated delivery")
	}
	if refreshing && (!sameReleasePickDelivery(existing.ReleasePickInput, r.ReleasePickInput) || existing.ReleasePickInput != r.ReleasePickInput && (existing.BaseSHA == r.BaseSHA || existing.HeadSHA == r.HeadSHA)) {
		return nativePublicInvalid("Refresh must retain the release delivery identity and replace its exact base and head")
	}
	if issue.Terminal || issue.Archived || issue.IgnoreDependencies || !slices.Contains(issue.Dependencies, r.SourceIssueID) {
		return nativePublicInvalid("Delivery must be an unfinished dependent issue with dependency admission enabled")
	}
	if publishing && (issue.IssueContract != nil && issue.IssueContract.HumanAction != "" || strings.EqualFold(issue.State, "Blocked") || !issue.DependencyReady || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch")) {
		return nativePublicInvalid("Held delivery cannot publish a release PR")
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if refreshing && strings.EqualFold(issue.State, landingLane) {
		if target := reworkTarget(project, issue.State); target == "" || target == issue.State {
			return nativePublicInvalid("Release refresh requires a workflow transition back to source capture")
		}
	}
	if publishing {
		var answered bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM issues i WHERE i.id = ? AND EXISTS (SELECT 1 "+nativeAnsweredAttemptFrom+"))", issueID).Scan(&answered); err != nil {
			return err
		}
		if answered {
			return nativePublicInvalid("Delivery retains an unanswered attempt hold")
		}
	}
	if publishing {
		for _, state := range project.States {
			if state.Name == issue.State && state.OperatorOnly {
				return nativePublicInvalid("Operator-only delivery cannot publish a release PR")
			}
		}
	}
	if !project.RequireDependencies {
		return nativePublicInvalid("Release delivery requires project dependency admission")
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?) AND released_at IS NULL AND expires_at > ?)`, item, formatHubTime(now)).Scan(&active); err != nil {
		return err
	}
	if active {
		return nativeConflict(issue.Revision)
	}
	source, _, err := readNativeIssue(ctx, tx, scope, string(r.SourceIssueID))
	if err != nil {
		return err
	}
	sourceChange, err := readChange(ctx, tx, scope, string(r.SourceIssueID), r.SourceChangeID)
	if err != nil {
		return err
	}
	landing := sourceChange.CurrentLanding()
	if !source.Terminal || sourceChange.WorkItemID != r.SourceIssueID || landing == nil || landing.BaseRef != "develop" || landing.MergeSHA != r.SourceMergeSHA {
		return nativePublicInvalid("Source dependency must have a recorded merged develop Change")
	}
	sourceVersion, err := readChangeVersion(ctx, tx, sourceChange.ID, landing.VersionID)
	if err != nil {
		return err
	}
	historicalSource := false
	if publishing {
		reviews, err := changeRows[tracker.ChangeReview](ctx, tx, "SELECT record_json FROM change_evidence WHERE change_id = ? AND version_id = ? AND kind = 'review' ORDER BY sequence DESC LIMIT 32", sourceChange.ID, sourceVersion.ID)
		if err != nil {
			return err
		}
		audited := false
		prNumber := 0
		if sourceVersion.External != nil {
			prNumber, err = strconv.Atoi(sourceVersion.External.ID)
			if err != nil || prNumber < 1 {
				return nativePublicInvalid("Source Change requires an exact PR identity")
			}
		}
		for _, review := range reviews {
			if review.VersionID != sourceVersion.ID {
				continue
			}
			if review.Decision == "changes_requested" {
				break
			}
			audit := review.SecurityAudit
			if audit == nil || audit.Landing {
				continue
			}
			key := securityaudit.Key{ProjectID: string(scope.project), Repository: sourceVersion.Repository, PRNumber: prNumber, BaseSHA: sourceVersion.BaseSHA, HeadSHA: sourceVersion.HeadSHA}
			evaluation := securityaudit.Evaluate(audit.Run, nil, key, "", []string{"p1", "p2"})
			if refreshing && evaluation.Reason == securityaudit.ReasonReviewerMismatch {
				evaluation = securityaudit.EvaluateLandedSource(audit.Run, key)
				historicalSource = evaluation.Allowed
			}
			audited = review.VersionID == sourceVersion.ID && review.Decision == "approved" && audit.Verdict == "pass" && audit.Run.Verdict == securityaudit.VerdictPass && audit.SessionID == audit.Run.ID && audit.SessionID > 0 && audit.Run.IssueID == string(r.SourceIssueID) && evaluation.Allowed
			break
		}
		if !audited {
			return nativePublicInvalid("Source Change requires a passing immutable security audit")
		}
	}
	connector, repositoryID, _, err := projectRepositoryBinding(ctx, tx, scope)
	if err != nil {
		return err
	}
	if connector == nil || r.Repository != "https://github.com/"+connector.Repository || sourceVersion.Repository != r.Repository {
		return nativePublicInvalid("Source and release delivery must share the project's configured repository")
	}
	prior, found, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
	if err != nil {
		return err
	}
	if found && prior.ID != "" && (!refreshing || prior.ID != existing.ChangeID || prior.Landed != nil) {
		return nativeConflict(issue.Revision)
	}
	if r.PRNumber > 0 {
		var conflict bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM release_deliveries WHERE repository = ? AND pr_number = ? AND work_item_id <> ?)`, r.Repository, r.PRNumber, item).Scan(&conflict); err != nil {
			return err
		}
		if conflict {
			return nativeConflict(issue.Revision)
		}
		if repositoryID == 0 {
			repositoryID, _, err = resolveWebhookRepositoryID(ctx, tx, connector.Repository)
			if err != nil {
				return err
			}
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE repository_id = ? AND github_number = ? AND issue_id IS NOT NULL AND issue_id <> (SELECT id FROM issues WHERE native_id = ?))`, repositoryID, r.PRNumber, item).Scan(&conflict); err != nil {
			return err
		}
		if conflict {
			return nativeConflict(issue.Revision)
		}

	}

	rules, err := readChangePolicy(ctx, tx, scope)
	if err != nil {
		return err
	}
	approved, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return err
	}
	independentReview := rules.RequireReview || approved.Policy.Gates.Validator || approved.Policy.Gates.SecurityAudit || approved.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired
	if historicalSource && (!approved.Policy.Gates.SecurityAudit || !rules.RequireReview && !approved.Policy.Gates.Validator && approved.Policy.Gates.AutomatedReview != gate.AutomatedReviewRequired) {
		return nativePublicInvalid("Historical source recovery requires current delivery security audit and independent scope review")
	}
	if !independentReview {
		return nativePublicInvalid("Release delivery requires the project's independent review policy")
	}
	return nil
}
