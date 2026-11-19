package changeapi

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

func (s *HTTP) HostedChangeCheckPrincipal(c echo.Context, credential hubapi.APICredential) bool {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	if !s.deps.ChangeCheckRequest(c) {
		return false
	}
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Project: tracker.ProjectID(c.Param("project")), Credential: credential}
	_, err := s.RequireHostedChangeCheckPrincipal(ctx, scope)
	return err == nil
}

func (s *HTTP) RequireHostedChangeCheckPrincipal(ctx context.Context, scope hubscope.NativeScope) (eventstore.ReviewPolicy, error) {
	var row eventstore.ReviewPolicy
	if s.deps.Repository == nil {
		return row, hubapi.NativeNotFound()
	}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.ReviewPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		return s.RecheckHostedChangeCheckPrincipal(ctx, r, scope, row)
	})
	if err != nil {
		return row, err
	}
	rules, err := eventstore.NewContent(s.deps.RunDetailStore()).ReviewPolicy(ctx, row)
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

func (s *HTTP) RecheckHostedChangeCheckPrincipal(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, approved eventstore.ReviewPolicy) error {
	credential := scope.Credential
	organization, hosted := s.deps.HostedOrganization()
	if !hosted || credential.Scope != hubapi.APIScopeOperator || credential.Hosted != nil || credential.Runner.RunnerID != "" || string(scope.Organization) != organization {
		return hubapi.NativeNotFound()
	}
	token, err := r.AccessCredential(ctx, credential.ID)
	if err != nil {
		return err
	}
	if token.Scope != string(hubapi.APIScopeOperator) || !token.Valid(credential.Hash, s.deps.Clock()) {
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
	if !s.deps.SameNativeClaimReviewPolicy(current, approved) {
		return hubapi.NativeNotFound()
	}
	return nil
}

// A hosted review is a project decision, so a write grant alone is not
// enough: the reviewer must also be an organization owner or administrator.
// Self-hosted operator tokens retain their existing review authority.
func (s *HTTP) RequireChangeReviewer() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.deps.RequireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeWorker)(func(c echo.Context) error {
			scope := hubscope.NativeRequestScope(c)
			if scope.Credential.Hosted != nil && scope.Credential.Scope != hubapi.APIScopeWorker {
				if scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" {
					return s.deps.NativeAPIError(c, hubapi.NativeNotFound())
				}
				scope.RequireHostedAdmin = true
				c.Set("native_scope", scope)
			}
			return next(c)
		})
	}
}

func (s *HTTP) RequireChangeReviewPolicyAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if _, hosted := s.deps.HostedOrganization(); !hosted {
			return s.deps.RequireInstanceAdmin()(s.deps.RequireNativeScope(hubapi.APIScopeAdmin)(next))
		}
		return s.deps.RequireOnboardingAdmin()(func(c echo.Context) error {
			scope := hubscope.NativeRequestScope(c)
			scope.RequireHostedAdmin = true
			c.Set("native_scope", scope)
			return next(c)
		})
	}
}

func (s *HTTP) GetChangeReviewPolicy(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	result, err := s.deps.ChangeReviewPolicyRecord(ctx, hubscope.NativeRequestScope(c))
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *HTTP) ApproveChangeReviewPolicy(c echo.Context) error {
	var request tracker.ApproveChangeReviewPolicy
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	return s.deps.ApproveChangeReviewPolicyRecord(c, request)
}

func (s *HTTP) ListChanges(c echo.Context) error {
	result, err := s.ReadChanges(c.Request().Context(), hubscope.NativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *HTTP) ReadChanges(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error) {
	return s.deps.ListChangeRecords(ctx, scope, item)
}

func (s *HTTP) CreateChange(c echo.Context) error {
	var request tracker.CreateChange
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.deps.InvalidAPIRequest(c, err)
	}
	return s.deps.CreateChangeRecord(c, request)
}

func (s *HTTP) GetChange(c echo.Context) error {
	if c.QueryParam("view") != "bounded" {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
		defer cancel()
		result, err := s.deps.SelectedChangeDetail(ctx, hubscope.NativeRequestScope(c), c.Param("item"), c.Param("change"), s.deps.Clock())
		if errors.Is(err, corestore.ErrInvalidPage) {
			err = &hubapi.NativeError{Code: "change_detail_too_large", Message: fmt.Sprintf("Change has more than %d versions or evidence records; read it with view=bounded", corestore.MaxPageSize), Status: http.StatusUnprocessableEntity, PublicMessage: true}
		}
		if err != nil {
			return s.deps.NativeAPIError(c, err)
		}
		return s.deps.NativeArchiveJSON(c, http.StatusOK, ChangeDetailResponse(result))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope, ctx := hubscope.NativeRequestScope(c), ctx
	var err error
	args := operatortool.ChangeArguments{ProjectID: string(scope.Project), ItemID: c.Param("item"), ChangeID: c.Param("change"), VersionID: c.QueryParam("version_id"), Section: c.QueryParam("section"), Cursor: c.QueryParam("cursor")}
	if c.QueryParam("limit") != "" {
		args.Limit, err = strconv.Atoi(c.QueryParam("limit"))
		if err != nil {
			return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("limit must be an integer"))
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	args, err = operatortool.DecodeChangeArguments(operatortool.GetChange, raw)
	if err != nil {
		return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("invalid Change read selector"))
	}
	result := s.deps.ChangeResult(scope, args)
	result, err = s.deps.ReadBoundedChangeRecord(ctx, scope, args, result)
	if err != nil {
		var refusal *operatortool.RequestError
		if errors.As(err, &refusal) {
			return s.deps.NativeAPIError(c, refusal)
		}
		if errors.Is(err, mutation.ErrConflict) {
			return s.deps.NativeAPIError(c, hubapi.NativeConflict(0))
		}
		if errors.Is(err, operatortool.ErrInvalidArguments) {
			return s.deps.NativeAPIError(c, hubapi.NativePublicInvalid("invalid Change read cursor"))
		}
		return s.deps.NativeAPIError(c, err)
	}
	if result.Detail != nil {
		detail := ChangeDetailResponse(*result.Detail)
		result.Detail = &detail
	}
	bounded, err := operatortool.BoundedChangeResult(result)
	if err != nil {
		return s.deps.NativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, "application/json", bounded.Content)
}

func ChangeDetailResponse(detail tracker.ChangeDetail) tracker.ChangeDetail {
	detail.Change.LinkedIssues = append([]tracker.NativeWorkItemID{}, detail.Change.LinkedIssues...)
	detail.Versions = append([]tracker.ChangeVersion{}, detail.Versions...)
	detail.Reviews = append([]tracker.ChangeReview{}, detail.Reviews...)
	detail.Checks = append([]tracker.ChangeCheck{}, detail.Checks...)
	detail.Discussion = append([]tracker.ChangeDiscussion{}, detail.Discussion...)
	detail.Summary.Messages = append([]string{}, detail.Summary.Messages...)
	for i := range detail.Versions {
		detail.Versions[i] = ChangeVersionResponse(detail.Versions[i])
	}
	for i := range detail.Checks {
		detail.Checks[i].Evidence = append([]tracker.ChangeArtifact{}, detail.Checks[i].Evidence...)
	}
	return detail
}

func ChangeVersionResponse(version tracker.ChangeVersion) tracker.ChangeVersion {
	version.Artifacts = append([]tracker.ChangeArtifact{}, version.Artifacts...)
	version.Checks = append([]tracker.ChangeCheckExpectation{}, version.Checks...)
	version.ReviewPolicy.RequiredChecks = append([]tracker.ChangeCheckSpec{}, version.ReviewPolicy.RequiredChecks...)
	return version
}
