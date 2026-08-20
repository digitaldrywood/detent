package runner

import (
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type AgentCapacityClassifier interface {
	ClassifyCapacityError(error, *telemetry.RateLimits, time.Time) (backendcapacity.Details, bool)
}

type AgentCapacityStatusProvider interface {
	CapacityStatus(*telemetry.RateLimits) (CapacityStatus, bool)
}

type CapacityStatus struct {
	Available bool
	Exhausted bool
	Detail    string
	Details   backendcapacity.Details
}

type CapacityController interface {
	CapacityScope(RunRequest) (backendcapacity.Scope, bool)
	ClassifyCapacityError(RunRequest, error, *telemetry.RateLimits, time.Time) (*backendcapacity.Error, bool)
}

type ValidatorCapacityController interface {
	ValidatorCapacityScope(ValidatorRequest) (backendcapacity.Scope, bool)
}

type CapacityStatusController interface {
	BackendCapacityStatus(backendcapacity.Scope, *telemetry.RateLimits) (CapacityStatus, bool)
}

func (r *Runner) CapacityScope(req RunRequest) (backendcapacity.Scope, bool) {
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	routeRole := runtime.effectiveRunRole(role)
	selection, _, backendConfig, err := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), routeRole)
	if err != nil {
		return backendcapacity.Scope{}, false
	}
	return configuredCapacityScope(selection, backendConfig, agentidentity.Identity{}), true
}

func (r *Runner) ClassifyCapacityError(
	req RunRequest,
	err error,
	rateLimits *telemetry.RateLimits,
	now time.Time,
) (*backendcapacity.Error, bool) {
	if err == nil {
		return nil, false
	}
	workflow, runtime, _, _ := r.runtimeSnapshot()
	role := runRole(req.Mode, req.Issue)
	routeRole := runtime.effectiveRunRole(role)
	selection, backend, backendConfig, selectionErr := runtime.selectBackendForRole(req.Issue, selectorContext(req.SelectorContext, workflow), routeRole)
	if selectionErr != nil {
		return nil, false
	}
	classified := classifyAgentCapacityError(backend, selection, backendConfig, agentidentity.Identity{}, err, rateLimits, now)
	capacityErr, ok := backendcapacity.As(classified)
	return capacityErr, ok
}

func (r *Runner) ValidatorCapacityScope(req ValidatorRequest) (backendcapacity.Scope, bool) {
	workflow, runtime, _, _ := r.runtimeSnapshot()
	selection, _, backendConfig, err := runtime.selectBackendForRole(
		req.Issue,
		selectorContext(req.SelectorContext, workflow),
		RoleValidator,
	)
	if err != nil {
		return backendcapacity.Scope{}, false
	}
	return configuredCapacityScope(selection, backendConfig, agentidentity.Identity{}), true
}

func (r *Runner) BackendCapacityStatus(
	scope backendcapacity.Scope,
	rateLimits *telemetry.RateLimits,
) (CapacityStatus, bool) {
	_, runtime, _, _ := r.runtimeSnapshot()
	backend, ok := runtime.backends[scope.Normalize().BackendID]
	if !ok {
		return CapacityStatus{}, false
	}
	provider, ok := backend.(AgentCapacityStatusProvider)
	if !ok {
		return CapacityStatus{}, false
	}
	return provider.CapacityStatus(rateLimits)
}

func classifyAgentCapacityError(
	backend AgentBackend,
	selection RouteSelection,
	backendConfig config.AgentBackend,
	identity agentidentity.Identity,
	err error,
	rateLimits *telemetry.RateLimits,
	now time.Time,
) error {
	var capacityErr *backendcapacity.Error
	if errors.As(err, &capacityErr) {
		return err
	}
	if err != nil {
		if classifier, ok := backend.(AgentCapacityClassifier); ok {
			scope := configuredCapacityScope(selection, backendConfig, identity)
			if scope.Hosted() {
				if details, classified := classifier.ClassifyCapacityError(err, rateLimits, now); classified {
					return backendcapacity.NewError(scope, details, err)
				}
			}
		}
	}
	scope := configuredCapacityScope(selection, backendConfig, identity)
	if !scope.Hosted() {
		return err
	}
	provider, ok := backend.(AgentCapacityStatusProvider)
	if !ok {
		return err
	}
	status, reported := provider.CapacityStatus(rateLimits)
	if !reported || !status.Exhausted {
		return err
	}
	details := status.Details
	if details.Type == "" {
		details.Type = backendcapacity.ErrorTypeUsageLimit
	}
	if strings.TrimSpace(details.Kind) == "" {
		details.Kind = "subscription_window_exhausted"
	}
	if strings.TrimSpace(details.Reason) == "" {
		details.Reason = "subscription window exhausted"
	}
	if err == nil {
		err = errors.New(firstNonBlankCapacityDetail(status.Detail, details.Reason))
	}
	return backendcapacity.NewError(scope, details, err)
}

func firstNonBlankCapacityDetail(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "subscription window exhausted"
}

func configuredCapacityScope(selection RouteSelection, backend config.AgentBackend, identity agentidentity.Identity) backendcapacity.Scope {
	provider := strings.TrimSpace(backend.Provider)
	if provider == "" && backend.Kind == config.AgentBackendCodex {
		provider = strings.TrimSpace(backend.CodexOptions().ModelProvider)
	}
	if provider == "" {
		provider = strings.TrimSpace(identity.Provider.Value)
	}
	return backendcapacity.Scope{
		BackendID:   selection.BackendID,
		BackendKind: backend.Kind,
		Provider:    provider,
	}.Normalize()
}

func IsCapacityError(err error) bool {
	var capacityErr *backendcapacity.Error
	return errors.Is(err, ErrWorkerGitHubRESTReserved) || errors.As(err, &capacityErr)
}

func IsTransientOverload(err error) bool {
	return backendcapacity.IsTransientOverload(err)
}
