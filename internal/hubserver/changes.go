package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const changeBase = hubapi.NativeBase + "/work-items/:item/changes"

func changeCheckRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == changeBase+"/:change/versions/:version/checks"
}

func (s *Service) hostedChangeCheckPrincipal(c echo.Context, credential hubapi.APICredential) bool {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if !changeCheckRequest(c) {
		return false
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project")), Credential: credential}
	_, err := s.requireHostedChangeCheckPrincipal(ctx, scope)
	return err == nil
}

func (s *Service) requireHostedChangeCheckPrincipal(ctx context.Context, scope hubscope.NativeScope) (eventstore.ReviewPolicy, error) {
	var row eventstore.ReviewPolicy
	if s.config.TenantRepository == nil {
		return row, hubapi.NativeNotFound()
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.ReviewPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		return s.recheckHostedChangeCheckPrincipal(ctx, r, scope, row)
	})
	if err != nil {
		return row, err
	}
	rules, err := eventstore.NewContent(s.runDetailStore()).ReviewPolicy(ctx, row)
	if err != nil {
		return row, err
	}
	for _, check := range rules.RequiredChecks {
		if check.PrincipalID == scope.Credential.ID && check.Source == "independent" {
			return row, nil
		}
	}
	return row, hubapi.NativeNotFound()
}

func (s *Service) recheckHostedChangeCheckPrincipal(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, approved eventstore.ReviewPolicy) error {
	credential := scope.Credential
	if s.config.Hosted == nil || credential.Scope != hubapi.APIScopeOperator || credential.Hosted != nil || credential.Runner.RunnerID != "" || string(scope.Organization) != s.config.Hosted.OrganizationID {
		return hubapi.NativeNotFound()
	}
	token, err := r.AccessCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	if token.Scope != string(hubapi.APIScopeOperator) || !token.Valid(credential.Hash, s.config.Clock()) {
		return hubapi.NativeNotFound()
	}
	if _, err := r.AccessTokenGrant(ctx, credential.ID, scope.Project, credential.HostedRole); err != nil {
		return err
	}
	if _, err := r.RunnerIdentityByCredential(ctx, credential.ID); !errors.Is(err, corestore.ErrNotFound) {
		if err != nil {
			return err
		}
		return hubapi.NativeNotFound()
	}
	current, err := r.ReviewPolicy(ctx, scope.Project)
	if err != nil {
		return err
	}
	if !sameNativeClaimReviewPolicy(current, approved) {
		return hubapi.NativeNotFound()
	}
	return nil
}

func (s *Service) registerChangeRoutes(e *echo.Echo) {
	read := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	write := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	operator := s.requireNativeScope(hubapi.APIScopeOperator)
	e.GET(hubapi.NativeBase+"/change-review-policy", s.getChangeReviewPolicy, read)
	e.PUT(hubapi.NativeBase+"/change-review-policy", s.approveChangeReviewPolicy, s.requireChangeReviewPolicyAdmin())
	e.GET(changeBase, s.listChanges, read)
	e.POST(changeBase, s.createChange, write)
	e.GET(changeBase+"/:change", s.getChange, read)
	e.POST(changeBase+"/:change/versions", s.publishChangeVersion, write)
	e.GET(changeBase+"/:change/versions/:version/source", s.changeSource, read)
	e.POST(changeBase+"/:change/discussion", s.discussChange, write)
	e.POST(changeBase+"/:change/versions/:version/reviews", s.reviewChange, s.requireChangeReviewer())
	e.GET(changeBase+"/:change/versions/:version/viewed-files", s.changeViewedFiles, operator)
	e.POST(changeBase+"/:change/versions/:version/viewed-files", s.viewChangeFile, operator)
	e.POST(changeBase+"/:change/versions/:version/checks", s.submitChangeCheck, write)
	e.POST(changeBase+"/:change/versions/:version/landing", s.landChange, write)
}

// A hosted review is a project decision, so a write grant alone is not
// enough: the reviewer must also be an organization owner or administrator.
// Self-hosted operator tokens retain their existing review authority.
func (s *Service) requireChangeReviewer() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeWorker)(func(c echo.Context) error {
			scope := hubscope.NativeRequestScope(c)
			if scope.Credential.Hosted != nil && scope.Credential.Scope != hubapi.APIScopeWorker {
				if scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" {
					return s.nativeAPIError(c, hubapi.NativeNotFound())
				}
				scope.RequireHostedAdmin = true
				c.Set("native_scope", scope)
			}
			return next(c)
		})
	}
}

func (s *Service) requireChangeReviewPolicyAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if s.config.Hosted == nil {
			return s.requireInstanceAdmin()(s.requireNativeScope(hubapi.APIScopeAdmin)(next))
		}
		return s.requireOnboardingAdmin()(func(c echo.Context) error {
			scope := hubscope.NativeRequestScope(c)
			scope.RequireHostedAdmin = true
			c.Set("native_scope", scope)
			return next(c)
		})
	}
}

func (s *Service) getChangeReviewPolicy(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	result, err := s.changeReviewPolicyRecord(ctx, hubscope.NativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) approveChangeReviewPolicy(c echo.Context) error {
	var request tracker.ApproveChangeReviewPolicy
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.approveChangeReviewPolicyRecord(c, request)
}

func (s *Service) listChanges(c echo.Context) error {
	result, err := s.readChanges(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readChanges(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error) {
	return s.listChangeRecords(ctx, scope, item)
}

func (s *Service) createChange(c echo.Context) error {
	var request tracker.CreateChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.createChangeRecord(c, request)
}

func (s *Service) getChange(c echo.Context) error {
	if c.QueryParam("view") != "bounded" {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
		defer cancel()
		result, err := s.selectedChangeDetail(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Param("change"), s.config.Clock())
		if errors.Is(err, corestore.ErrInvalidPage) {
			err = &hubapi.NativeError{Code: "change_detail_too_large", Message: fmt.Sprintf("Change has more than %d versions or evidence records; read it with view=bounded", corestore.MaxPageSize), Status: http.StatusUnprocessableEntity, PublicMessage: true}
		}
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return nativeArchiveJSON(c, http.StatusOK, changeDetailResponse(result))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope, ctx := hubscope.NativeRequestScope(c), ctx
	var err error
	args := operatortool.ChangeArguments{ProjectID: string(scope.Project), ItemID: c.Param("item"), ChangeID: c.Param("change"), VersionID: c.QueryParam("version_id"), Section: c.QueryParam("section"), Cursor: c.QueryParam("cursor")}
	if c.QueryParam("limit") != "" {
		args.Limit, err = strconv.Atoi(c.QueryParam("limit"))
		if err != nil {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("limit must be an integer"))
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	args, err = operatortool.DecodeChangeArguments(operatortool.GetChange, raw)
	if err != nil {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("invalid Change read selector"))
	}
	result := (hubChangeApplication{service: s}).result(scope, args)
	if c.QueryParam("projection") == "publication_source" {
		result, err = s.readPublicationSource(ctx, scope, args, result)
	} else {
		result, err = s.readBoundedChangeRecord(ctx, scope, args, result)
	}
	if err != nil {
		var refusal *operatortool.RequestError
		if errors.As(err, &refusal) {
			return s.nativeAPIError(c, refusal)
		}
		if errors.Is(err, mutation.ErrConflict) {
			return s.nativeAPIError(c, hubapi.NativeConflict(0))
		}
		if errors.Is(err, operatortool.ErrInvalidArguments) {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("invalid Change read cursor"))
		}
		return s.nativeAPIError(c, err)
	}
	if result.Detail != nil {
		detail := changeDetailResponse(*result.Detail)
		result.Detail = &detail
	}
	bounded, err := operatortool.BoundedChangeResult(result)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/json", bounded.Content)
}

func changeDetailResponse(detail tracker.ChangeDetail) tracker.ChangeDetail {
	detail.Change.LinkedIssues = append([]tracker.NativeWorkItemID{}, detail.Change.LinkedIssues...)
	detail.Versions = append([]tracker.ChangeVersion{}, detail.Versions...)
	detail.Reviews = append([]tracker.ChangeReview{}, detail.Reviews...)
	detail.Checks = append([]tracker.ChangeCheck{}, detail.Checks...)
	detail.Discussion = append([]tracker.ChangeDiscussion{}, detail.Discussion...)
	detail.Summary.Messages = append([]string{}, detail.Summary.Messages...)
	for i := range detail.Versions {
		detail.Versions[i] = changeVersionResponse(detail.Versions[i])
	}
	for i := range detail.Checks {
		detail.Checks[i].Evidence = append([]tracker.ChangeArtifact{}, detail.Checks[i].Evidence...)
	}
	return detail
}

func changeVersionResponse(version tracker.ChangeVersion) tracker.ChangeVersion {
	version.Artifacts = append([]tracker.ChangeArtifact{}, version.Artifacts...)
	version.Checks = append([]tracker.ChangeCheckExpectation{}, version.Checks...)
	version.ReviewPolicy.RequiredChecks = append([]tracker.ChangeCheckSpec{}, version.ReviewPolicy.RequiredChecks...)
	return version
}
