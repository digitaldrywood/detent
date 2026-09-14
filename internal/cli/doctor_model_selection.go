package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

func checkDoctorModelSelection(id string, cfg workflowconfig.Config) doctorCheck {
	policy := cfg.EffectiveModelSelection()
	check := doctorCheck{Name: "Project " + id + " agent selection", Status: doctorOK}
	if problems := policy.Validate(); len(problems) > 0 {
		check.Status = doctorFail
		check.Detail = strings.Join(problems, "; ")
		return check
	}
	state := "disabled"
	if policy.Active() {
		state = "enabled"
	}
	details := []string{"automatic model selection: " + state}
	for _, backend := range cfg.AgentBackendConfigs() {
		source := cfg.Agents.Sources["backends."+backend.ID]
		if source == "" {
			source = "project"
		}
		details = append(details, fmt.Sprintf("backend %s (%s): %s", backend.ID, backend.Kind, source))
	}
	for _, route := range cfg.AgentRouteConfigs() {
		source := cfg.Agents.Sources["routes."+route.Name]
		if source == "" {
			source = "project"
		}
		details = append(details, fmt.Sprintf("route %s: %s, backend=%s, model=%s", route.Name, source, route.Backend, route.Model))
	}
	keys := make([]string, 0, len(policy.Sources))
	for key := range policy.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		details = append(details, key+"="+policy.Sources[key])
	}
	if policy.Configured() {
		encoded, err := json.Marshal(policy)
		if err != nil {
			check.Status = doctorFail
			check.Detail = "encode effective model policy: " + err.Error()
			return check
		}
		details = append(details, "effective policy: "+string(encoded))
	}
	details = append(details, "pricing: standard API USD estimates; subscription charges, credits, Fast mode, long context, and cache-write premiums can differ")
	check.Detail = strings.Join(details, "; ")
	return check
}

func checkDoctorBackendModelCatalogs(ctx context.Context, id string, cfg workflowconfig.Config, deps doctorDeps) []doctorCheck {
	backends := cfg.AgentBackendConfigs()
	checks := make([]doctorCheck, 0, len(backends))
	for _, backend := range backends {
		name := fmt.Sprintf("Project %s backend %s model catalog", id, backend.ID)
		if strings.TrimSpace(backend.Kind) != workflowconfig.AgentBackendCodex {
			checks = append(checks, doctorCheck{
				Name:   name,
				Status: doctorOK,
				Detail: fmt.Sprintf("backend %s (%s) does not advertise a model catalog; probe skipped", backend.ID, backend.Kind),
			})
			continue
		}
		connector.ReportProgress(ctx)
		count, err := deps.modelCatalogProbe(ctx, backend)
		if err != nil {
			checks = append(checks, doctorCheck{
				Name:   name,
				Status: doctorFail,
				Detail: fmt.Sprintf("backend %s (%s) model catalog unavailable: %s", backend.ID, backend.Kind, runnerpkg.CatalogErrorDiagnostic(err)),
				Hint:   "Fix the backend command, working directory, trust, authentication, or transport failure, then rerun detent doctor.",
			})
			continue
		}
		checks = append(checks, doctorCheck{
			Name:   name,
			Status: doctorOK,
			Detail: fmt.Sprintf("listed %d model(s) from backend %s (%s)", count, backend.ID, backend.Kind),
		})
	}
	return checks
}

func defaultDoctorBackendModelCatalogProbe(ctx context.Context, cfg workflowconfig.AgentBackend) (int, error) {
	backend, err := buildAgentBackend(cfg)
	if err != nil {
		return 0, err
	}
	provider, ok := backend.(runnerpkg.AgentModelCatalogProvider)
	if !ok {
		return 0, errors.New("backend does not advertise a model catalog")
	}
	models, err := provider.ListModels(ctx)
	if err != nil {
		return 0, err
	}
	return len(models), nil
}
