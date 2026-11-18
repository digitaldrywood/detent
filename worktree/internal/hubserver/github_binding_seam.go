package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/githubbinding"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) githubBindingService(scope hubscope.NativeScope) *githubbinding.Service {
	return githubbinding.New(githubbinding.Deps{
		Repository: s.config.TenantRepository, Now: s.config.Clock, Backend: s.config.ReconcileBackend,
		Recheck: func(ctx context.Context, _ hubapi.Scope) error { return s.recheckProjectRecordMutation(ctx, scope) },
		Receipt: func(ctx context.Context, _ hubapi.Scope, operation, key, hash string) (json.RawMessage, bool, error) {
			return s.authorizedChangeRecordReceipt(ctx, scope, operation, key, hash)
		},
		ReadProject: func(ctx context.Context, _ hubapi.Scope) (projectstore.Record, error) {
			return s.readProjectRecord(ctx, scope)
		},
		Mutate: func(ctx context.Context, _ hubapi.Scope, operation string, mutation tracker.Mutation, input, value any, apply func(tenantstore.Writer) error) (json.RawMessage, error) {
			return s.tenantReceiptMutation(ctx, scope, operation, mutation, input, value, apply)
		},
		RequireFeature: func(ctx context.Context, r tenantstore.Reader, feature string, now time.Time) error {
			return hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, feature, now))
		},
	})
}

func (s *Service) bindNativeRepositoryCommand(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, request bindNativeRepositoryRequest) (json.RawMessage, error) {
	raw, err := s.githubBindingService(scope).Bind(ctx, s.authorization().Scope(scope), options.OperationID, request)
	return raw, hubapi.FromFeatureError(err)
}

type bindNativeRepositoryRequest = githubbinding.Request

func (s *Service) bindNativeRepository(c echo.Context) error {
	var request bindNativeRepositoryRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	result, err := s.bindNativeRepositoryCommand(c.Request().Context(), hubscope.NativeRequestScope(c), hubapi.NativeCommandOptions{OperationID: c.Request().Method + " " + c.Request().URL.EscapedPath(), Feature: hostedMutationFeature(c)}, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func validCheckoutRepository(repository string) bool {
	return runnerstore.ValidCheckoutRepository(repository)
}
func (s *Service) verifiedProjectCheckout(ctx context.Context, _ hubapi.NativeQueryer, scope hubscope.NativeScope, repository string, now time.Time) (bool, error) {
	return s.githubBindingService(scope).VerifiedCheckout(ctx, s.authorization().Scope(scope), repository, now)
}
func validateRunnerCheckoutReport(scope hubscope.NativeScope, report *string) error {
	return hubapi.FromFeatureError(githubbinding.ValidateCheckoutReport(scope.Credential.Runner.RunnerID, report))
}
func repositoryBindingRefusal(code, message string) error {
	return hubapi.FromFeatureError(githubbinding.Refusal(code, message))
}
