package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrReleasePickMismatch = errors.New("release pick provenance mismatch")

type ReleasePickVerifier interface {
	VerifyReleasePick(context.Context, tracker.ReleasePickInput) error
	VerifyReleasePickRefresh(context.Context, tracker.ReleasePickInput, tracker.ReleasePickInput) error
}

type ReleaseDeliveryVerifier interface {
	ReleaseDeliveryStatus(context.Context, tracker.ReleasePickInput) (tracker.ReleaseDeliveryStatus, error)
}

func (s *Service) getReleaseDeliveryStatus(c echo.Context) error {
	ctx := c.Request().Context()
	scope := nativeRequestScope(c)
	issue, err := s.readReleaseDeliveryIssue(ctx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if issue.ReleaseDelivery == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	verifier, ok := s.config.ReconcileBackend.(ReleaseDeliveryVerifier)
	if !ok {
		return s.nativeAPIError(c, errors.New("release repository transport is unavailable"))
	}
	status, err := verifier.ReleaseDeliveryStatus(ctx, issue.ReleaseDelivery.ReleasePickInput)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	current, err := s.readReleaseDeliveryIssue(ctx, scope, c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if current.Revision != issue.Revision || current.ReleaseDelivery == nil || *current.ReleaseDelivery != *issue.ReleaseDelivery {
		return s.nativeAPIError(c, nativeConflict(current.Revision))
	}
	return c.JSON(http.StatusOK, status)
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
	if s.config.TenantRepository == nil {
		return workItemStorageUnavailable()
	}
	connector, _, _, err := s.projectRepositoryBinding(ctx, nil, scope)
	if err != nil {
		return err
	}
	if connector == nil || r.Repository != "https://github.com/"+connector.Repository {
		return nativePublicInvalid("Release PR repository must match the project's configured repository")
	}
	existing, found, err := s.readReleaseDeliveryRecord(ctx, scope, item)
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
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	raw, err := s.associateReleasePickTenant(ctx, nativeRequestScope(c), c.Param("item"), c.Request().Method+" "+c.Request().URL.EscapedPath(), r)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.pullRequests.invalidate(pullRequestCacheKey(nativeRequestScope(c), c.Param("item")))
	return c.JSONBlob(http.StatusOK, raw)
}
