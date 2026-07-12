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

type CapacityController interface {
	CapacityScope(RunRequest) (backendcapacity.Scope, bool)
	ClassifyCapacityError(RunRequest, error, *telemetry.RateLimits, time.Time) (*backendcapacity.Error, bool)
}

type ValidatorCapacityController interface {
	ValidatorCapacityScope(ValidatorRequest) (backendcapacity.Scope, bool)
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

func classifyAgentCapacityError(
	backend AgentBackend,
	selection RouteSelection,
	backendConfig config.AgentBackend,
	identity agentidentity.Identity,
	err error,
	rateLimits *telemetry.RateLimits,
	now time.Time,
) error {
	if err == nil {
		return nil
	}
	classifier, ok := backend.(AgentCapacityClassifier)
	if !ok {
		return err
	}
	scope := configuredCapacityScope(selection, backendConfig, identity)
	if !scope.Hosted() {
		return err
	}
	details, ok := classifier.ClassifyCapacityError(err, rateLimits, now)
	if !ok {
		return err
	}
	return backendcapacity.NewError(scope, details, err)
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
	return errors.As(err, &capacityErr)
}

func IsTransientOverload(err error) bool {
	return backendcapacity.IsTransientOverload(err)
}
