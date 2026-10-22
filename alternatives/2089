package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrReleasePickMismatch = errors.New("release pick provenance mismatch")

type ReleasePickVerifier interface {
	VerifyReleasePick(context.Context, tracker.ReleasePickInput) error
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
	if r.HeadSHA == r.BaseSHA || r.HeadRef == r.BaseRef || r.SourceMergeSHA == r.FrozenSHA {
		return nativePublicInvalid("Release pick must identify a distinct source fix and PR head")
	}
	if !validNativeID(string(r.SourceIssueID), "wi") || !validNativeID(r.SourceChangeID, "change") || r.ExpectedRevision < 1 || r.PRNumber < 1 || !releaseBranchPattern.MatchString(r.BaseRef) || !validReleaseHeadRef(r.HeadRef) {
		return nativePublicInvalid("Release delivery requires a source issue and Change, revision, PR number, pick branch and frozen release branch")
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
	if err := verifier.VerifyReleasePick(ctx, r.ReleasePickInput); err != nil {
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
		return nil, nativeConflict(issue.Revision)
	}
	if err := requireNativeEdit(issue, r.ExpectedRevision); err != nil {
		return nil, err
	}
	if issue.Terminal || issue.Archived || issue.IgnoreDependencies || !slices.Contains(issue.Dependencies, r.SourceIssueID) {
		return nil, nativePublicInvalid("Delivery must be an unfinished dependent issue with dependency admission enabled")
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	if !project.RequireDependencies {
		return nil, nativePublicInvalid("Release delivery requires project dependency admission")
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?) AND released_at IS NULL AND expires_at > ?)`, item, formatHubTime(now)).Scan(&active); err != nil {
		return nil, err
	}
	if active {
		return nil, nativeConflict(issue.Revision)
	}
	source, _, err := readNativeIssue(ctx, tx, scope, string(r.SourceIssueID))
	if err != nil {
		return nil, err
	}
	sourceChange, err := readChange(ctx, tx, scope, string(r.SourceIssueID), r.SourceChangeID)
	if err != nil {
		return nil, err
	}
	landing := sourceChange.CurrentLanding()
	if !source.Terminal || sourceChange.WorkItemID != r.SourceIssueID || landing == nil || landing.BaseRef != "develop" || landing.MergeSHA != r.SourceMergeSHA {
		return nil, nativePublicInvalid("Source dependency must have a recorded merged develop Change")
	}
	sourceVersion, err := readChangeVersion(ctx, tx, sourceChange.ID, landing.VersionID)
	if err != nil {
		return nil, err
	}
	connector, repositoryID, _, err := projectRepositoryBinding(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	if connector == nil || r.Repository != "https://github.com/"+connector.Repository || sourceVersion.Repository != r.Repository {
		return nil, nativePublicInvalid("Source and release delivery must share the project's configured repository")
	}
	prior, found, err := readLatestNativeChangeRequest(ctx, tx, scope, item)
	if err != nil {
		return nil, err
	}
	if found && prior.ID != "" {
		return nil, nativeConflict(issue.Revision)
	}
	var conflict bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM release_deliveries WHERE repository_id = ? AND pr_number = ?)`, repositoryID, r.PRNumber).Scan(&conflict); err != nil {
		return nil, err
	}
	if conflict {
		return nil, nativeConflict(issue.Revision)
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pull_requests WHERE repository_id = ? AND github_number = ? AND issue_id IS NOT NULL AND issue_id <> (SELECT id FROM issues WHERE native_id = ?))`, repositoryID, r.PRNumber, item).Scan(&conflict); err != nil {
		return nil, err
	}
	if conflict {
		return nil, nativeConflict(issue.Revision)
	}

	rules, err := readChangePolicy(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	approved, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return nil, err
	}
	independentReview := rules.RequireReview || approved.Policy.Gates.Validator || approved.Policy.Gates.SecurityAudit || approved.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired
	if !independentReview {
		return nil, nativePublicInvalid("Release delivery requires the project's independent review policy")
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
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item, scope.organization, scope.project, d.SourceIssueID, d.SourceChangeID, d.SourceMergeSHA, repositoryID,
		d.Repository, d.PRNumber, d.HeadRef, d.HeadSHA, d.BaseRef, d.BaseSHA, d.FrozenSHA, d.ChangeID, nil, d.Actor.PrincipalID, d.Actor.Kind, d.Actor.ViaKey, formatHubTime(now))
	if err != nil {
		return nil, err
	}
	_, err = persistNativeIssue(ctx, tx, scope, issue, "issue.edited", tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: d.ChangeID, VersionID: d.VersionID, HeadSHA: d.HeadSHA}}, now)
	return d, err
}
