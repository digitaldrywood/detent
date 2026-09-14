package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestDoctorModelSelectionProvenance(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			cfg := workflowconfig.Default()
			cfg.Agents.ModelSelection.ComplexModel = new("project-complex")
			cfg.Agents.ModelSelection.Enabled = &enabled
			host := workflowconfig.Agents{
				Backends:       []workflowconfig.AgentBackend{{ID: "design", Kind: workflowconfig.AgentBackendClaudeCode, Command: "SECRET_ENV=value /private/secret-wrapper"}},
				Routes:         []workflowconfig.AgentRoute{{Name: "design", Backend: "design", Model: "sonnet"}},
				ModelSelection: workflowconfig.ModelSelection{Preset: new("sol_first"), NormalModel: new("host-normal")},
			}
			got := checkDoctorModelSelection("alpha", cfg.WithAgentDefaults(host, workflowconfig.AgentBudgetDefaults{}))
			if got.Status != doctorOK {
				t.Fatalf("check = %+v", got)
			}
			for _, want := range []string{"normal_model=instance", "complex_model=project", "levels.normal.effort=preset", "backend design (claude_code): instance", "route design: instance", "subscription charges"} {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail missing %q", want)
				}
			}
			for _, secret := range []string{"SECRET_ENV", "secret-wrapper", "/private"} {
				if strings.Contains(got.Detail, secret) {
					t.Errorf("detail leaks %q", secret)
				}
			}
		})
	}
}

func TestCheckDoctorBackendModelCatalogs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		backends    []workflowconfig.AgentBackend
		probeErrors map[string]error
		want        map[string]struct {
			status doctorStatus
			detail string
		}
		wantProbed []string
	}{
		{
			name: "probes every Codex backend",
			backends: []workflowconfig.AgentBackend{
				doctorCodexAgentBackend("codex-code"),
				doctorCodexAgentBackend("codex-plan"),
			},
			want: map[string]struct {
				status doctorStatus
				detail string
			}{
				"codex-code": {status: doctorOK, detail: "listed 3 model(s)"},
				"codex-plan": {status: doctorOK, detail: "listed 3 model(s)"},
			},
			wantProbed: []string{"codex-code", "codex-plan"},
		},
		{
			name: "reports underlying catalog error",
			backends: []workflowconfig.AgentBackend{
				doctorCodexAgentBackend("codex-code"),
			},
			probeErrors: map[string]error{
				"codex-code": errors.New("initialize codex app-server: model/list response: permission denied"),
			},
			want: map[string]struct {
				status doctorStatus
				detail string
			}{
				"codex-code": {status: doctorFail, detail: "initialize codex app-server: model/list response: permission denied"},
			},
			wantProbed: []string{"codex-code"},
		},
		{
			name: "non catalog backend is explicit",
			backends: []workflowconfig.AgentBackend{
				doctorClaudeCodeAgentBackend("claude-code"),
			},
			want: map[string]struct {
				status doctorStatus
				detail string
			}{
				"claude-code": {status: doctorOK, detail: "does not advertise a model catalog; probe skipped"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := validDoctorWorkflowWithBackends(t.TempDir(), tc.backends...)
			var probed []string
			progress := 0
			ctx := connector.WithProgressReporter(t.Context(), func() { progress++ })
			checks := checkDoctorBackendModelCatalogs(ctx, "alpha", cfg, doctorDeps{
				modelCatalogProbe: func(_ context.Context, backend workflowconfig.AgentBackend) (int, error) {
					probed = append(probed, backend.ID)
					return 3, tc.probeErrors[backend.ID]
				},
			})
			if len(checks) != len(tc.want) {
				t.Fatalf("checks = %#v, want %d", checks, len(tc.want))
			}
			for backendID, expected := range tc.want {
				name := "Project alpha backend " + backendID + " model catalog"
				check := doctorCheckByName(t, doctorReport{Checks: checks}, name)
				if check.Status != expected.status || !strings.Contains(check.Detail, expected.detail) {
					t.Fatalf("check %q = %#v, want status %s detail containing %q", name, check, expected.status, expected.detail)
				}
			}
			if strings.Join(probed, ",") != strings.Join(tc.wantProbed, ",") {
				t.Fatalf("probed backends = %v, want %v", probed, tc.wantProbed)
			}
			if progress != len(tc.wantProbed) {
				t.Fatalf("progress signals = %d, want one for each of %d backend probes", progress, len(tc.wantProbed))
			}
		})
	}
}

type doctorCatalogResponseError struct {
	message string
	body    string
}

func (e *doctorCatalogResponseError) Error() string { return e.message + ": " + e.body }

func (e *doctorCatalogResponseError) BackendErrorMessage() string { return e.message }

func TestCheckDoctorBackendModelCatalogsSanitizesBackendResponseBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{name: "bounded message", message: "models/list rejected " + strings.Repeat("é", 300), want: "models/list rejected"},
		{name: "empty message", want: "without diagnostic detail"},
		{name: "whitespace message", message: " \n\t ", want: "without diagnostic detail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secret := "private-response-body"
			cfg := validDoctorWorkflowWithBackends(t.TempDir(), doctorCodexAgentBackend("codex"))
			checks := checkDoctorBackendModelCatalogs(t.Context(), "alpha", cfg, doctorDeps{
				modelCatalogProbe: func(context.Context, workflowconfig.AgentBackend) (int, error) {
					return 0, &doctorCatalogResponseError{message: tc.message, body: secret}
				},
			})

			check := doctorCheckByName(t, doctorReport{Checks: checks}, "Project alpha backend codex model catalog")
			if strings.Contains(check.Detail, secret) || !strings.Contains(check.Detail, tc.want) {
				t.Fatalf("catalog check detail = %q, want bounded backend message without response body", check.Detail)
			}
		})
	}
}

func TestCheckDoctorProjectReportsBackendModelCatalogError(t *testing.T) {
	t.Parallel()

	cfg := validDoctorWorkflow(t.TempDir())
	deps := successfulDoctorDeps()
	deps.loadWorkflow = func(string) (workflowconfig.Workflow, error) {
		return workflowconfig.Workflow{Config: cfg}, nil
	}
	deps.modelCatalogProbe = func(_ context.Context, backend workflowconfig.AgentBackend) (int, error) {
		if backend.ID != workflowconfig.DefaultAgentBackendID {
			t.Fatalf("backend ID = %q, want %q", backend.ID, workflowconfig.DefaultAgentBackendID)
		}
		return 0, errors.New("model/list response: catalog transport closed")
	}

	checks := checkDoctorProject(t.Context(), globalconfig.Project{
		ID:       "alpha",
		Workflow: "WORKFLOW.md",
		Workdir:  cfg.Workspace.Root,
	}, deps, RuntimeSecret{}, false)
	check := doctorCheckByName(t, doctorReport{Checks: checks}, "Project alpha backend codex model catalog")
	if check.Status != doctorFail || !strings.Contains(check.Detail, "model/list response: catalog transport closed") {
		t.Fatalf("catalog check = %#v, want failed check with ListModels error", check)
	}
}
