package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func selectionCatalog() []AgentModel {
	return []AgentModel{
		{ID: "gpt-5.6-sol", Model: "gpt-5.6-sol", SupportedReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-6-astra", Model: "gpt-6-astra", Default: true, SupportedReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}},
	}
}

func TestSelectionReloadAndResume(t *testing.T) {
	backend := &catalogAgentBackend{models: selectionCatalog()}
	cfg := config.Default()
	cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
	runner, err := NewRunner(Dependencies{Workflow: config.Workflow{Config: cfg}, Workspace: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, AgentBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	initial, oldRuntime, _, _ := runner.runtimeSnapshot()
	identity := agentidentity.Configured("codex", "codex", "default", RoleCode, "gpt-5.6-sol", "", "medium", "", time.Now())
	identity.Selection = agentidentity.Selection{Policy: "sol_first", Reason: "default_complexity"}
	resume := RunRequest{RetryMode: RetryModeResume, ResumeState: store.AgentResumeState{ProviderThreadID: "thread-1", RuntimeIdentity: identity}}
	cfg.Agents.ModelSelection.NormalModel = new("gpt-6-astra")
	if err := runner.UpdateWorkflowChecked(config.Workflow{Config: cfg}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		req  RunRequest
		want string
	}{
		{name: "new dispatch", want: "gpt-6-astra"},
		{name: "resumed dispatch", req: resume, want: "gpt-5.6-sol"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			capacity, err := runner.DispatchCapacity(context.Background(), tt.req)
			if err != nil || capacity.Model != tt.want {
				t.Fatalf("capacity=%+v error=%v", capacity, err)
			}
		})
	}
	if initial.Config.EffectiveModelSelection().Model("normal") != "gpt-5.6-sol" || oldRuntime.backends["codex"] != backend {
		t.Fatal("active snapshot changed")
	}
	backend.err = errors.New("catalog unavailable")
	got := resolveRequestAgentSelection(context.Background(), resume, AgentProcessRequest{}, "gpt-6-astra", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
	if got.Err != nil || got.Model != "gpt-5.6-sol" || got.Effort != "medium" {
		t.Fatalf("resume = %+v", got)
	}
	_, _, _, err = (agentRuntime{}).selectRequestBackend(resume, selector.Context{}, RoleCode)
	if err == nil {
		t.Fatal("missing resume backend accepted")
	}
	bad := cfg
	bad.Agents.ModelSelection.DefaultLevel = new("missing")
	if err := runner.UpdateWorkflowChecked(config.Workflow{Config: bad}); err == nil {
		t.Fatal("invalid reload accepted")
	}
	lastGood, _, _, _ := runner.runtimeSnapshot()
	if *lastGood.Config.EffectiveModelSelection().DefaultLevel != "normal" {
		t.Fatal("invalid reload replaced last known good config")
	}
}

func TestCustomSelectionSettings(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		mutate                    func(*config.ModelSelection)
		kind, role, model, effort string
	}{
		{name: "disabled", mutate: func(p *config.ModelSelection) { p.Enabled = new(false) }, model: "", effort: ""},
		{name: "cleared eligible backends", mutate: func(p *config.ModelSelection) { p.BackendKinds = &[]string{} }, model: "", effort: ""},
		{name: "custom backend excluded", kind: config.AgentBackendClaudeCode, model: "", effort: ""},
		{name: "cleared fallback", mutate: func(p *config.ModelSelection) { p.FallbackOrder = &[]string{} }, model: "gpt-5.6-sol", effort: "medium"},
		{name: "stage defaults", mutate: func(p *config.ModelSelection) {
			p.Stages = map[string]config.ModelSelectionStage{RoleMerge: {Model: new("complex"), Effort: new("high")}}
		}, role: RoleMerge, model: "gpt-6-astra", effort: "high"},
		{name: "role rule", mutate: func(p *config.ModelSelection) {
			p.Rules = &[]config.ModelSelectionRule{{Name: "merge-complex", Roles: []string{RoleMerge}, Level: "complex", Selector: selector.Selector{Fields: []selector.FieldEquals{{Name: "Complex", Value: "yes"}}}}}
		}, role: RoleMerge, model: "gpt-6-astra", effort: "medium"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.ModelSelection.Preset = new("sol_first")
			if tt.mutate != nil {
				tt.mutate(&cfg.Agents.ModelSelection)
			}
			kind, role := tt.kind, tt.role
			if kind == "" {
				kind = config.AgentBackendCodex
			}
			if role == "" {
				role = RoleCode
			}
			backend := &catalogAgentBackend{models: selectionCatalog()}
			got := resolveAgentSelection(context.Background(), connector.Issue{Fields: map[string]string{"Complex": "yes"}}, AgentProcessRequest{}, "", role, cfg, config.AgentBackend{Kind: kind}, backend)
			if got.Err != nil || got.Model != tt.model || got.Effort != tt.effort {
				t.Fatalf("selection=%+v", got)
			}
		})
	}
}

func TestAutomaticModelSelection(t *testing.T) {
	for _, tt := range []struct {
		name, body, role, label, base, projectEffort, model, effort string
	}{
		{name: "missing metadata", model: "gpt-5.6-sol", effort: "medium"},
		{name: "generic enhancement", label: "enhancement", model: "gpt-5.6-sol", effort: "medium"},
		{name: "high effort", body: "effort: high", model: "gpt-5.6-sol", effort: "high"},
		{name: "complex metadata", label: "complexity:complex", model: "gpt-6-astra", effort: "medium"},
		{name: "very complex", label: "complexity:very-complex", model: "gpt-6-astra", effort: "high"},
		{name: "model only", body: "model: gpt-6-astra", model: "gpt-6-astra", effort: "medium"},
		{name: "explicit low", body: "effort: low", label: "complexity:very-complex", model: "gpt-6-astra", effort: "low"},
		{name: "xhigh signal", body: "effort: xhigh", model: "gpt-6-astra", effort: "medium"},
		{name: "max signal", body: "effort: max", model: "gpt-6-astra", effort: "medium"},
		{name: "both explicit", body: "model: gpt-5.6-sol\neffort: max", label: "complexity:very-complex", model: "gpt-5.6-sol", effort: "high"},
		{name: "role fields", body: "model: gpt-6-astra\neffort: high\ncode:\n  model: gpt-5.6-sol\n  effort: low", model: "gpt-5.6-sol", effort: "low"},
		{name: "role model issue effort", body: "effort: xhigh\ncode:\n  model: gpt-5.6-sol", model: "gpt-5.6-sol", effort: "medium"},
		{name: "role effort issue model", body: "model: gpt-5.6-sol\ncode:\n  effort: xhigh", model: "gpt-5.6-sol", effort: "medium"},
		{name: "code inherited in rework", role: RoleRework, body: "code:\n  model: gpt-6-astra\n  effort: low", model: "gpt-6-astra", effort: "low"},
		{name: "entering rework", role: RoleRework, model: "gpt-5.6-sol", effort: "medium"},
		{name: "routine stays normal", role: RoleRoutine, label: "complexity:very-complex", body: "effort: xhigh", model: "gpt-5.6-sol", effort: "medium"},
		{name: "merge stays normal", role: RoleMerge, label: "complexity:very-complex", model: "gpt-5.6-sol", effort: "medium"},
		{name: "validator stays normal", role: RoleValidator, label: "complexity:complex", model: "gpt-5.6-sol", effort: "medium"},
		{name: "role specific merge signal", role: RoleMerge, body: "merge:\n  effort: xhigh", model: "gpt-6-astra", effort: "medium"},
		{name: "route pin", base: "gpt-5.6-sol", label: "complexity:very-complex", model: "gpt-5.6-sol", effort: "high"},
		{name: "issue wins route", base: "gpt-6-astra", body: "model: gpt-5.6-sol", model: "gpt-5.6-sol", effort: "medium"},
		{name: "project effort not complexity", projectEffort: "xhigh", model: "gpt-5.6-sol", effort: "medium"},
		{name: "issue effort wins project", projectEffort: "high", body: "effort: low", model: "gpt-5.6-sol", effort: "low"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
			cfg.Agent.Effort.Code = tt.projectEffort
			issue := connector.Issue{Priority: new(1), State: "Rework"}
			if tt.body != "" {
				issue.Description = "```detent-agent\nschema: 1\n" + tt.body + "\n```"
			}
			if tt.label != "" {
				issue.Labels = []string{tt.label}
			}
			backend := &catalogAgentBackend{models: selectionCatalog(), defaultModel: "gpt-6-astra"}
			role := tt.role
			if role == "" {
				role = RoleCode
			}
			got := resolveAgentSelection(context.Background(), issue, AgentProcessRequest{Workspace: t.TempDir()}, tt.base, role, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
			if got.Err != nil || got.Model != tt.model || got.Effort != tt.effort {
				t.Fatalf("selection = %+v, want %s %s", got, tt.model, tt.effort)
			}
			if backend.defaultCalls != 0 || backend.calls != 1 {
				t.Fatalf("catalog/default calls = %d/%d", backend.calls, backend.defaultCalls)
			}
			if got.Selection.Reason == "" || got.Selection.ModelSource == "" || got.Selection.EffortSource == "" {
				t.Fatalf("missing provenance: %+v", got.Selection)
			}
		})
	}
}

func TestAutomaticModelSelectionFailures(t *testing.T) {
	for _, tt := range []struct {
		name, body, unavailable     string
		catalog                     []AgentModel
		catalogErr                  error
		fallbackReason              string
		fallback, failure, rejected bool
		clearFallbackOrder          bool
	}{
		{name: "Astra unavailable", catalog: selectionCatalog()[:1], fallback: true},
		{name: "Astra retired", catalog: []AgentModel{selectionCatalog()[0], {ID: "gpt-6-astra", Upgrade: "replacement"}}, fallback: true},
		{name: "neither available", failure: true},
		{name: "catalog unavailable", catalogErr: errors.New("catalog transport details"), fallback: true, fallbackReason: "automatic model selection: model catalog unavailable: catalog transport details"},
		{name: "catalog unavailable with fail configured", catalogErr: errors.New("catalog transport details"), unavailable: "fail", failure: true},
		{name: "catalog unavailable with empty fallback order", catalogErr: errors.New("catalog transport details"), clearFallbackOrder: true, failure: true},
		{name: "catalog unavailable with explicit model", catalogErr: errors.New("catalog transport details"), body: "model: gpt-6-astra", failure: true},
		{name: "fail configured", catalog: selectionCatalog()[:1], unavailable: "fail", failure: true},
		{name: "invalid explicit model", catalog: selectionCatalog(), body: "model: absent", failure: true, rejected: true},
		{name: "invalid explicit effort", catalog: selectionCatalog(), body: "effort: absent", failure: true, rejected: true},
		{name: "invalid role does not downgrade", catalog: selectionCatalog(), body: "effort: low\ncode:\n  effort: absent", failure: true, rejected: true},
		{name: "malformed override", catalog: selectionCatalog(), body: "unknown: value", failure: true, rejected: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
			if tt.unavailable != "" {
				cfg.Agents.ModelSelection.Unavailable = &tt.unavailable
			}
			if tt.clearFallbackOrder {
				cfg.Agents.ModelSelection.FallbackOrder = &[]string{}
			}
			issue := connector.Issue{Labels: []string{"complexity:complex"}}
			if tt.body != "" {
				issue.Description = "```detent-agent\nschema: 1\n" + tt.body + "\n```"
			}
			backend := &catalogAgentBackend{models: tt.catalog, err: tt.catalogErr}
			got := resolveAgentSelection(context.Background(), issue, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
			if (got.Err != nil) != tt.failure || (len(got.Rejections) > 0) != tt.rejected {
				t.Fatalf("selection = %+v", got)
			}
			if (got.Selection.FallbackReason != "") != tt.fallback {
				t.Fatalf("fallback = %+v", got.Selection)
			}
			if tt.fallbackReason != "" && got.Selection.FallbackReason != tt.fallbackReason {
				t.Fatalf("fallback reason = %q, want %q", got.Selection.FallbackReason, tt.fallbackReason)
			}
			if tt.fallback && (got.Model != "gpt-5.6-sol" || got.Selection.RequestedModel != "gpt-6-astra") {
				t.Fatalf("fallback identity = %+v", got)
			}
		})
	}
}

type catalogResponseError struct {
	message string
	body    string
}

func (e *catalogResponseError) Error() string { return e.message + ": " + e.body }

func (e *catalogResponseError) BackendErrorMessage() string { return e.message }

func TestCatalogErrorDiagnosticOmitsResponseBodyAndBoundsMessage(t *testing.T) {
	t.Parallel()

	secret := strings.Repeat("é", 300)
	got := catalogErrorDiagnostic(&catalogResponseError{message: "models/list rejected " + secret, body: `{"token":"private"}`})
	if strings.Contains(got, "private") || len(got) > 515 || !strings.HasSuffix(got, "...") || strings.ToValidUTF8(got, "") != got {
		t.Fatalf("catalogErrorDiagnostic() = %q", got)
	}
}

func TestConfiguredEffortCeiling(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		effort string
		resume bool
		want   string
	}{
		{name: "legacy xhigh", effort: "xhigh", want: "medium"},
		{name: "legacy max", effort: "max", want: "medium"},
		{name: "supported high", effort: "high", want: "high"},
		{name: "resumed legacy xhigh", effort: "xhigh", resume: true, want: "medium"},
		{name: "resumed operator reduction", effort: "medium", resume: true, want: "medium"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first"), NormalModel: new("gpt-6-astra"), Levels: map[string]config.ModelSelectionDefaults{"normal": {Effort: new("low")}}}
			req := RunRequest{Issue: connector.Issue{Description: "```detent-agent\nschema: 1\neffort: " + test.effort + "\n```"}}
			if test.resume {
				identity := agentidentity.Configured("codex", "codex", "default", RoleCode, "gpt-6-astra", "", "xhigh", "", time.Now())
				req.RetryMode = RetryModeResume
				req.ResumeState = store.AgentResumeState{ProviderThreadID: "thread-1", RuntimeIdentity: identity}
			}
			backend := &catalogAgentBackend{models: selectionCatalog()}
			got := resolveRequestAgentSelection(t.Context(), req, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
			if got.Err != nil || got.Model != "gpt-6-astra" || got.Effort != test.want {
				t.Fatalf("selection = %+v, want Astra effort %s", got, test.want)
			}
		})
	}
}

func TestResumeCatalogFailureKeepsPublicRejectionGeneric(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
	identity := agentidentity.Configured("codex", "codex", "default", RoleCode, "gpt-6-astra", "", "xhigh", "", time.Now())
	req := RunRequest{
		Issue:       connector.Issue{Description: "```detent-agent\nschema: 1\neffort: medium\n```"},
		RetryMode:   RetryModeResume,
		ResumeState: store.AgentResumeState{ProviderThreadID: "thread-1", RuntimeIdentity: identity},
	}
	diagnostic := errors.New("stderr: failed to initialize sqlite state runtime")
	got := resolveRequestAgentSelection(t.Context(), req, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, &catalogAgentBackend{err: diagnostic})
	if got.Err == nil || !strings.Contains(got.Err.Error(), diagnostic.Error()) {
		t.Fatalf("attempt error = %v, want diagnostic %q", got.Err, diagnostic)
	}
	if len(got.Rejections) != 1 || strings.Contains(got.Rejections[0].Reason, diagnostic.Error()) {
		t.Fatalf("public rejections = %#v, want one sanitized reason", got.Rejections)
	}
}

func TestRunnerResumeUsesBoundedEffort(t *testing.T) {
	cfg := config.Default()
	cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
	backend := &fakeCodexClient{models: selectionCatalog(), result: AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-2", SessionID: "thread-1-turn-2"}}
	sessionStore := &fakeSessionStore{sessionID: 1}
	runner, err := NewRunner(Dependencies{
		ProjectID:    "detent",
		Workflow:     config.Workflow{Config: cfg, Prompt: "Work"},
		Workspace:    &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
		AgentBackend: backend,
		Store:        sessionStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := agentidentity.Configured("codex", "codex", "default", RoleCode, "gpt-6-astra", "", "xhigh", "", time.Now())
	_, err = runner.Run(t.Context(), RunRequest{
		Issue:       connector.Issue{ID: "issue-1", Identifier: "repo#1", Description: "```detent-agent\nschema: 1\neffort: medium\n```"},
		RetryMode:   RetryModeResume,
		ResumeState: store.AgentResumeState{ProviderThreadID: "thread-1", RuntimeIdentity: identity},
	})
	if err != nil {
		t.Fatal(err)
	}
	if backend.request.Model != "gpt-6-astra" || backend.request.ReasoningEffort != "medium" || backend.request.Resume.ThreadID != "thread-1" {
		t.Fatalf("resumed request model=%s effort=%s thread=%s", backend.request.Model, backend.request.ReasoningEffort, backend.request.Resume.ThreadID)
	}
	if sessionStore.started.RuntimeIdentity.ReasoningEffort.Value != "medium" {
		t.Fatalf("persisted start effort=%+v", sessionStore.started.RuntimeIdentity.ReasoningEffort)
	}
}

func TestRunnerCatalogFailureUsesConfiguredNormalModel(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first"), NormalModel: new("gpt-5.6-sol")}
	backend := &fakeCodexClient{
		catalogErr: errors.New("app-server models/list failed"),
		result:     AgentTurnResult{ThreadID: "thread-1", TurnID: "turn-1", SessionID: "thread-1-turn-1"},
	}
	sessions := &fakeSessionStore{sessionID: 1}
	var logs bytes.Buffer
	runner, err := NewRunner(Dependencies{
		ProjectID:    "detent",
		Workflow:     config.Workflow{Config: cfg, Prompt: "Work"},
		Workspace:    &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}},
		AgentBackend: backend,
		Store:        sessions,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(t.Context(), RunRequest{
		Issue: connector.Issue{ID: "issue-1", Identifier: "repo#1", Labels: []string{"complexity:complex"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	wantReason := "automatic model selection: model catalog unavailable: app-server models/list failed"
	if backend.request.Model != "gpt-5.6-sol" {
		t.Fatalf("request model = %q, want configured normal model", backend.request.Model)
	}
	selection := sessions.started.RuntimeIdentity.Selection
	if selection.RequestedModel != "gpt-6-astra" || selection.FallbackReason != wantReason {
		t.Fatalf("persisted selection = %+v", selection)
	}
	if !strings.Contains(logs.String(), `model_selection_fallback_reason="`+wantReason+`"`) {
		t.Fatalf("worker log did not record catalog fallback: %s", logs.String())
	}
}

func TestUltracodeEffortCeiling(t *testing.T) {
	t.Parallel()
	for _, resume := range []bool{false, true} {
		cfg := config.Default()
		cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
		cfg.Agent.Effort.Code = "ultracode"
		req := RunRequest{}
		if resume {
			req.RetryMode = RetryModeResume
			req.ResumeState = store.AgentResumeState{ProviderThreadID: "thread-1", RuntimeIdentity: agentidentity.Configured("codex", "codex", "default", RoleCode, "gpt-5.6-sol", "", "ultracode", "", time.Now())}
		}
		catalog := selectionCatalog()
		catalog[0].SupportedReasoningEfforts = append(catalog[0].SupportedReasoningEfforts, "ultracode")
		got := resolveRequestAgentSelection(t.Context(), req, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, &catalogAgentBackend{models: catalog})
		if got.Err != nil || got.Effort != "medium" {
			t.Fatalf("resume=%t selection=%+v, want medium effort", resume, got)
		}
	}
}

func TestEffortCeilingPolicyBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		mutate     func(*config.ModelSelection)
		body       string
		role       string
		kind       string
		catalog    []AgentModel
		resume     bool
		catalogErr error
		want       string
		wantError  bool
	}{
		{name: "policy permits xhigh", mutate: func(p *config.ModelSelection) {
			p.Levels = map[string]config.ModelSelectionDefaults{"complex": {Effort: new("xhigh")}}
		}, body: "effort: xhigh", want: "xhigh"},
		{name: "stage permits xhigh", mutate: func(p *config.ModelSelection) {
			p.Stages = map[string]config.ModelSelectionStage{RoleMerge: {Effort: new("xhigh")}}
		}, role: RoleMerge, body: "effort: xhigh", want: "xhigh"},
		{name: "other stage does not raise ceiling", mutate: func(p *config.ModelSelection) {
			p.Stages = map[string]config.ModelSelectionStage{RoleMerge: {Effort: new("xhigh")}}
		}, body: "effort: xhigh", want: "medium"},
		{name: "disabled", mutate: func(p *config.ModelSelection) { p.Enabled = new(false) }, body: "model: gpt-6-astra\neffort: max", want: "max"},
		{name: "excluded backend", kind: config.AgentBackendClaudeCode, body: "model: gpt-6-astra\neffort: xhigh", want: "xhigh"},
		{name: "fallback stays bounded", catalog: selectionCatalog()[:1], body: "effort: xhigh", want: "medium"},
		{name: "resume no issue override", resume: true, want: "medium"},
		{name: "resume role override", resume: true, body: "code:\n  effort: low", want: "low"},
		{name: "resume malformed override", resume: true, body: "effort: unknown", wantError: true},
		{name: "resume changed effort catalog unavailable", resume: true, catalogErr: errors.New("unavailable"), wantError: true},
		{name: "resume changed effort unsupported", resume: true, catalog: []AgentModel{{ID: "gpt-6-astra", Model: "gpt-6-astra", SupportedReasoningEfforts: []string{"xhigh"}}}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Agents.ModelSelection = config.ModelSelection{Preset: new("sol_first")}
			if test.mutate != nil {
				test.mutate(&cfg.Agents.ModelSelection)
			}
			req := RunRequest{}
			if test.body != "" {
				req.Issue.Description = "```detent-agent\nschema: 1\n" + test.body + "\n```"
			}
			if test.resume {
				req.RetryMode = RetryModeResume
				req.ResumeState = store.AgentResumeState{ProviderThreadID: "thread-1", RuntimeIdentity: agentidentity.Configured("codex", "codex", "default", RoleCode, "gpt-6-astra", "", "xhigh", "", time.Now())}
			}
			catalog := test.catalog
			if catalog == nil {
				catalog = selectionCatalog()
			}
			role, kind := test.role, test.kind
			if role == "" {
				role = RoleCode
			}
			if kind == "" {
				kind = config.AgentBackendCodex
			}
			backend := &catalogAgentBackend{models: catalog, err: test.catalogErr}
			got := resolveRequestAgentSelection(t.Context(), req, AgentProcessRequest{}, "", role, cfg, config.AgentBackend{Kind: kind}, backend)
			if (got.Err != nil) != test.wantError || !test.wantError && got.Effort != test.want {
				t.Fatalf("selection=%+v want effort=%s error=%v", got, test.want, test.wantError)
			}
		})
	}
}

func TestIssueConfigurationErrorClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		effort            string
		catalogError      error
		wantConfiguration bool
		wantError         bool
	}{
		{name: "invalid issue effort", effort: "normal", wantConfiguration: true, wantError: true},
		{name: "corrected effort", effort: "low"},
		{name: "catalog unavailable falls back", effort: "low", catalogError: errors.New("catalog unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Agents.ModelSelection.Preset = new("sol_first")
			backend := &catalogAgentBackend{models: selectionCatalog(), err: tc.catalogError}
			issue := connector.Issue{Description: "```detent-agent\nschema: 1\neffort: " + tc.effort + "\n```"}
			selection := resolveRequestAgentSelection(t.Context(), RunRequest{Issue: issue}, AgentProcessRequest{}, "", RoleCode, cfg, config.AgentBackend{Kind: config.AgentBackendCodex}, backend)
			var invalid *IssueConfigurationError
			if errors.As(selection.Err, &invalid) != tc.wantConfiguration || (selection.Err != nil) != tc.wantError {
				t.Fatalf("classification = %T (%v), want configuration %v, error %v", selection.Err, selection.Err, tc.wantConfiguration, tc.wantError)
			}
		})
	}
}
