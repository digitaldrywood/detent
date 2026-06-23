package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestOnboardingRoutesProgressThroughWizard(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding, WorkflowPath: workflowPath}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name        string
		method      string
		path        string
		form        url.Values
		wantStatus  int
		wantContent []string
	}{
		{
			name:       "start",
			method:     http.MethodGet,
			path:       "/onboarding",
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Detent onboarding",
				"Choose tracker",
				onboardingStepBadge("tracker"),
				"ProjectV2 board",
				"Organization issue field",
				"Repository labels",
				"hx-post=\"/onboarding/tracker\"",
			},
		},
		{
			name:       "tracker to credentials",
			method:     http.MethodPost,
			path:       "/onboarding/tracker",
			form:       url.Values{"tracker_choice": {"github_label"}},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Credentials",
				onboardingStepBadge("credentials"),
				"name=\"tracker_kind\" value=\"github\"",
				"name=\"github_status_source\" value=\"label\"",
				"name=\"api_key\"",
				"value=\"$GITHUB_TOKEN\"",
			},
		},
		{
			name:   "credentials to project",
			method: http.MethodPost,
			path:   "/onboarding/credentials",
			form: url.Values{
				"tracker_kind": {"github"},
				"endpoint":     {"https://api.github.com/graphql"},
				"api_key":      {"$GITHUB_TOKEN"},
			},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Pick project",
				onboardingStepBadge("project"),
				"name=\"project_slug\"",
				"name=\"repo\"",
			},
		},
		{
			name:   "project to agent config",
			method: http.MethodPost,
			path:   "/onboarding/project",
			form: url.Values{
				"tracker_kind": {"github"},
				"endpoint":     {"https://api.github.com/graphql"},
				"api_key":      {"$GITHUB_TOKEN"},
				"project_slug": {"PVT_project"},
				"repo":         {"digitaldrywood/detent"},
			},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Agent config",
				onboardingStepBadge("agent"),
				"name=\"delivery_profile\"",
				"Autonomous delivery mode",
				"name=\"max_concurrent_agents\"",
				"name=\"workspace_root\"",
				"name=\"dependency_auto_unblock_enabled\"",
				"name=\"dispatch_priority_by_label\"",
			},
		},
		{
			name:   "agent config to write",
			method: http.MethodPost,
			path:   "/onboarding/agent",
			form: url.Values{
				"tracker_kind":               {"github"},
				"endpoint":                   {"https://api.github.com/graphql"},
				"api_key":                    {"$GITHUB_TOKEN"},
				"project_slug":               {"PVT_project"},
				"repo":                       {"digitaldrywood/detent"},
				"workspace_root":             {"~/code/detent-workspaces"},
				"max_concurrent_agents":      {"5"},
				"max_turns":                  {"20"},
				"polling_interval_ms":        {"120000"},
				"merging_concurrency":        {"1"},
				"dispatch_priority_by_state": {"Merging\nRework"},
				"dispatch_priority_by_label": {"bug\nregression\nenhancement"},
			},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Write WORKFLOW.md",
				onboardingStepBadge("write"),
				"ProjectV2",
				"hx-post=\"/onboarding/write\"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := onboardingRequest(tt.method, tt.path, tt.form)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			for _, content := range tt.wantContent {
				if !strings.Contains(rec.Body.String(), content) {
					t.Fatalf("body missing %q:\n%s", content, rec.Body.String())
				}
			}
		})
	}
}

func TestOnboardingGitHubProjectStepIsModeSpecific(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding, WorkflowPath: workflowPath}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name     string
		source   string
		want     []string
		unwanted []string
	}{
		{
			name:   "project v2",
			source: workflowconfig.GitHubStatusSourceProjectV2,
			want: []string{
				"Pick project",
				"ProjectV2 ID",
				"name=\"project_slug\"",
				"name=\"repo\"",
			},
			unwanted: []string{
				"name=\"status_field\"",
				"name=\"status_label_prefix\"",
			},
		},
		{
			name:   "issue field",
			source: workflowconfig.GitHubStatusSourceIssueField,
			want: []string{
				"Configure issue field",
				"name=\"repo\"",
				"name=\"status_field\"",
				"value=\"Status\"",
			},
			unwanted: []string{
				"ProjectV2 ID",
				"name=\"project_slug\"",
				"name=\"status_label_prefix\"",
			},
		},
		{
			name:   "label",
			source: workflowconfig.GitHubStatusSourceLabel,
			want: []string{
				"Configure labels",
				"name=\"repo\"",
				"name=\"status_label_prefix\"",
				"value=\"detent:\"",
			},
			unwanted: []string{
				"ProjectV2 ID",
				"name=\"project_slug\"",
				"name=\"status_field\"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := onboardingRequest(http.MethodPost, "/onboarding/credentials", url.Values{
				"tracker_kind":         {"github"},
				"github_status_source": {tt.source},
				"endpoint":             {"https://api.github.com/graphql"},
				"api_key":              {"$GITHUB_TOKEN"},
			})

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(rec.Body.String(), unwanted) {
					t.Fatalf("body contains %q:\n%s", unwanted, rec.Body.String())
				}
			}
		})
	}
}

func onboardingStepBadge(step string) string {
	return `data-onboarding-step-badge="true"><span class="text-xs font-medium uppercase text-muted-foreground">Step</span> <span class="truncate font-mono text-xs font-semibold text-foreground">` + step + `</span>`
}

func TestOnboardingWriteGitHubWorkflows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		source      string
		edit        func(url.Values)
		want        []string
		unwanted    []string
		wantProject string
		wantRepo    string
		wantField   string
		wantPrefix  string
	}{
		{
			name:   "project v2",
			source: workflowconfig.GitHubStatusSourceProjectV2,
			want: []string{
				"tracker:\n  kind: github",
				"  github_status_source: project_v2",
				"project_slug: PVT_project",
				"You are working on GitHub issue `{{ issue.identifier }}` in ProjectV2 `PVT_project`.",
			},
			unwanted: []string{
				"repository: digitaldrywood/detent",
				"status_field:",
				"status_label_prefix:",
			},
			wantProject: "PVT_project",
		},
		{
			name:   "issue field",
			source: workflowconfig.GitHubStatusSourceIssueField,
			edit: func(form url.Values) {
				form.Del("project_slug")
				form.Set("status_field", "Team: Status #2")
			},
			want: []string{
				"tracker:\n  kind: github",
				"  github_status_source: issue_field",
				"repository: digitaldrywood/detent",
				`status_field: "Team: Status #2"`,
				"You are working on GitHub issue `{{ issue.identifier }}` with issue-field status in `digitaldrywood/detent`.",
			},
			unwanted: []string{
				"project_slug:",
				"status_label_prefix:",
				"ProjectV2",
			},
			wantRepo:  "digitaldrywood/detent",
			wantField: "Team: Status #2",
		},
		{
			name:   "label",
			source: workflowconfig.GitHubStatusSourceLabel,
			edit: func(form url.Values) {
				form.Del("project_slug")
				form.Set("status_label_prefix", "detent:")
			},
			want: []string{
				"tracker:\n  kind: github",
				"  github_status_source: label",
				"repository: digitaldrywood/detent",
				`status_label_prefix: "detent:"`,
				"You are working on GitHub issue `{{ issue.identifier }}` with status labels in `digitaldrywood/detent`.",
			},
			unwanted: []string{
				"project_slug:",
				"status_field:",
				"ProjectV2",
			},
			wantRepo:   "digitaldrywood/detent",
			wantPrefix: "detent:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
			server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			form := validOnboardingForm()
			form.Set("github_status_source", tt.source)
			if tt.edit != nil {
				tt.edit(form)
			}
			rec := httptest.NewRecorder()
			req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "Wrote WORKFLOW.md") {
				t.Fatalf("body missing success:\n%s", rec.Body.String())
			}

			raw, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			content := string(raw)
			sourceRoot := filepath.Dir(workflowPath)
			for _, want := range append(tt.want,
				"api_key: $GITHUB_TOKEN",
				"dependency_auto_unblock:\n    enabled: false\n    source_states:\n      - Blocked\n    target_state: Todo\n    readiness: terminal_or_merged",
				"source_root: "+sourceRoot,
				"codex:\n  command: codex app-server",
				"gate:\n  kind: command\n  run: make check\n  ci_failure_action: skip",
				"validator:\n    enabled: false\n    model: \"\"\n    min_score: 0.8\n    block_on:\n      - p1",
				"hooks:\n  timeout_ms: 60000",
				"max_concurrent_agents_by_state:\n    Merging: 1",
				"dispatch_priority_by_label:\n    - bug\n    - regression\n    - enhancement",
				"You are working on GitHub issue `{{ issue.identifier }}`",
			) {
				if !strings.Contains(content, want) {
					t.Fatalf("workflow missing %q:\n%s", want, content)
				}
			}
			for _, unwanted := range append(tt.unwanted,
				"codex:\n  command: codex app-server\n  shell:",
				"hooks:\n  shell:",
				"after_create:",
				"git clone",
				"git worktree add",
			) {
				if strings.Contains(content, unwanted) {
					t.Fatalf("workflow contains %q:\n%s", unwanted, content)
				}
			}

			workflow, err := workflowconfig.ParseWorkflow(raw)
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			cfg := workflow.Config
			if cfg.Tracker.GitHubStatusSource != tt.source {
				t.Fatalf("GitHubStatusSource = %q, want %q", cfg.Tracker.GitHubStatusSource, tt.source)
			}
			if cfg.Tracker.ProjectSlug != tt.wantProject {
				t.Fatalf("ProjectSlug = %q, want %q", cfg.Tracker.ProjectSlug, tt.wantProject)
			}
			if cfg.Tracker.Repository != tt.wantRepo {
				t.Fatalf("Repository = %q, want %q", cfg.Tracker.Repository, tt.wantRepo)
			}
			if tt.wantField != "" && cfg.Tracker.StatusField != tt.wantField {
				t.Fatalf("StatusField = %q, want %q", cfg.Tracker.StatusField, tt.wantField)
			}
			if tt.wantPrefix != "" && cfg.Tracker.StatusLabelPrefix != tt.wantPrefix {
				t.Fatalf("StatusLabelPrefix = %q, want %q", cfg.Tracker.StatusLabelPrefix, tt.wantPrefix)
			}
		})
	}
}

func TestOnboardingWriteLabelWorkflowKanbanMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		edit     func(url.Values)
		wantMode string
	}{
		{
			name:     "operator default recommends integration",
			wantMode: workflowconfig.KanbanModeIntegration,
		},
		{
			name: "observer choice stays read only",
			edit: func(form url.Values) {
				form.Set("kanban_mode", workflowconfig.KanbanModeReadOnly)
			},
			wantMode: workflowconfig.KanbanModeReadOnly,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
			server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}

			form := validOnboardingForm()
			form.Set("github_status_source", workflowconfig.GitHubStatusSourceLabel)
			form.Set("status_label_prefix", "detent:")
			form.Del("project_slug")
			if tt.edit != nil {
				tt.edit(form)
			}

			rec := httptest.NewRecorder()
			req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			raw, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			content := string(raw)
			wantContent := "server:\n  kanban:\n    mode: " + tt.wantMode
			if !strings.Contains(content, wantContent) {
				t.Fatalf("workflow missing %q:\n%s", wantContent, content)
			}
			if !strings.Contains(content, "detent doctor --allow-write-probes") {
				t.Fatalf("workflow missing write-probe guidance:\n%s", content)
			}

			workflow, err := workflowconfig.ParseWorkflow(raw)
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			if workflow.Config.Server.Kanban.Mode != tt.wantMode {
				t.Fatalf("Kanban mode = %q, want %q", workflow.Config.Server.Kanban.Mode, tt.wantMode)
			}
		})
	}
}

func TestOnboardingAgentStepExplainsKanbanWriteProbeRequirement(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding, WorkflowPath: workflowPath}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/project", url.Values{
		"tracker_kind":         {"github"},
		"github_status_source": {workflowconfig.GitHubStatusSourceLabel},
		"endpoint":             {"https://api.github.com/graphql"},
		"api_key":              {"$GITHUB_TOKEN"},
		"repo":                 {"digitaldrywood/detent"},
		"status_label_prefix":  {"detent:"},
	})

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"name=\"kanban_mode\" value=\"integration\" checked",
		"operator-owned local/private installs",
		"detent doctor --allow-write-probes",
		"name=\"kanban_mode\" value=\"read_only\"",
		"observer/shared dashboards",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestOnboardingProjectStepPreservesKanbanModeAfterAgentBack(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding, WorkflowPath: workflowPath}, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := validOnboardingForm()
	form.Set("github_status_source", workflowconfig.GitHubStatusSourceLabel)
	form.Set("status_label_prefix", "detent:")
	form.Set("kanban_mode", workflowconfig.KanbanModeReadOnly)
	form.Set("delivery_profile", "autonomous_delivery")
	form.Del("project_slug")

	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/credentials", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"Configure labels",
		"name=\"kanban_mode\" value=\"read_only\"",
		"name=\"delivery_profile\" value=\"autonomous_delivery\"",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}

	rec = httptest.NewRecorder()
	req = onboardingRequest(http.MethodPost, "/onboarding/project", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("agent status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"name=\"kanban_mode\" value=\"read_only\" checked",
		"name=\"delivery_profile\" value=\"autonomous_delivery\" checked",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("agent body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestOnboardingWriteWorkflowCanEnableDependencyAutoUnblock(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := validOnboardingForm()
	form.Set("dependency_auto_unblock_enabled", "true")
	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), "dependency_auto_unblock:\n    enabled: true") {
		t.Fatalf("workflow missing enabled dependency auto-unblock:\n%s", raw)
	}
}

func TestOnboardingWriteWorkflowAppliesAutonomousDeliveryProfile(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := validOnboardingForm()
	form.Set("delivery_profile", "autonomous_delivery")
	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gates and CI still apply") {
		t.Fatalf("body missing autonomous profile explanation:\n%s", rec.Body.String())
	}
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		"dependency_auto_unblock:\n    enabled: true",
		"max_concurrent_agents_by_state:\n    Merging: 1",
		"auto_promote:\n    enabled: true\n    quiet_seconds: 0",
		"gate:\n  kind: command\n  run: make check\n  require_automated_review: false",
		"server:\n  host: 127.0.0.1\n  kanban:\n    mode: integration",
		"Autonomous delivery still requires linked PRs, green CI, and clear gates.",
		"Use live reload or a project-scoped refresh after onboarding changes; do not restart Detent or interrupt running agents unless the operator explicitly authorizes it.",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("workflow missing %q:\n%s", want, content)
		}
	}

	workflow, err := workflowconfig.ParseWorkflow(raw)
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	cfg := workflow.Config
	if !cfg.Agent.AutoPromote.Enabled || cfg.Agent.AutoPromote.QuietSeconds != 0 {
		t.Fatalf("AutoPromote = %#v, want enabled with no quiet wait", cfg.Agent.AutoPromote)
	}
	if !cfg.Tracker.DependencyAutoUnblock.Enabled {
		t.Fatal("DependencyAutoUnblock.Enabled = false, want true")
	}
	if cfg.Agent.MaxConcurrentAgentsByState["merging"] != 1 {
		t.Fatalf("Merging concurrency = %d, want 1", cfg.Agent.MaxConcurrentAgentsByState["merging"])
	}
	if cfg.Server.Kanban.Mode != workflowconfig.KanbanModeIntegration {
		t.Fatalf("Kanban mode = %q, want integration", cfg.Server.Kanban.Mode)
	}
	if cfg.Gate.RequireAutomatedReview == nil || *cfg.Gate.RequireAutomatedReview {
		t.Fatalf("RequireAutomatedReview = %v, want false", cfg.Gate.RequireAutomatedReview)
	}
}

func TestOnboardingWriteMemoryWorkflowSeedsSampleIssue(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/write", validMemoryOnboardingForm())

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(raw)
	for _, want := range []string{
		"tracker:\n  kind: memory",
		"issues:\n    - id: memory-onboarding-1",
		"identifier: MEM-1",
		"state: Todo",
		"You are working on a memory tracker issue `{{ issue.identifier }}`",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("workflow missing %q:\n%s", want, content)
		}
	}
}

func TestOnboardingWriteRunsCloseoutVerifierAfterMutation(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	snapshots := hub.New[telemetry.Snapshot]()
	beforeRefresh := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	requestedAt := beforeRefresh.Add(30 * time.Second)
	afterRefresh := beforeRefresh.Add(time.Minute)
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: beforeRefresh,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &beforeRefresh,
			},
		}},
		Refresh: telemetry.Refresh{
			Status:        telemetry.RefreshStatusReady,
			LastRefreshAt: &beforeRefresh,
		},
		BoardIssues: []telemetry.Issue{
			{
				ID:         "issue-todo",
				Identifier: "digitaldrywood/detent#646",
				ProjectID:  "detent",
				State:      "Todo",
				Labels:     []string{"detent:todo", "enhancement"},
			},
			{
				ID:         "issue-review",
				Identifier: "digitaldrywood/detent#645",
				ProjectID:  "detent",
				State:      "Human Review",
				Labels:     []string{"detent:human-review"},
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Registry = project.NewRegistry()
	mustSetOnboardingProject(t, deps.Registry, "detent", workflowPath)
	refresher := &publishSnapshotRefreshProbe{
		hub: snapshots,
		response: web.RefreshResponse{
			Queued:      true,
			RequestedAt: requestedAt,
			Operations:  []string{"poll", "reconcile"},
		},
		snapshot: telemetry.Snapshot{
			GeneratedAt: afterRefresh,
			Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Projects: []telemetry.ProjectSnapshot{{
				Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
				Counts:  telemetry.Counts{Running: 1},
				Refresh: telemetry.Refresh{
					Status:        telemetry.RefreshStatusReady,
					LastRefreshAt: &afterRefresh,
				},
			}},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &afterRefresh,
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "issue-todo",
					Identifier: "digitaldrywood/detent#646",
					ProjectID:  "detent",
					State:      "Todo",
					Labels:     []string{"detent:todo", "enhancement"},
				},
				{
					ID:         "issue-review",
					Identifier: "digitaldrywood/detent#645",
					ProjectID:  "detent",
					State:      "Human Review",
					Labels:     []string{"detent:human-review"},
				},
			},
			Running: []telemetry.Running{{
				Issue: telemetry.Issue{
					ID:         "issue-running",
					Identifier: "digitaldrywood/detent#647",
					ProjectID:  "detent",
					State:      "In Progress",
					Labels:     []string{"detent:in-progress"},
					Comments: []telemetry.IssueComment{{
						Body: "## Codex Workpad\n\n### Status\nIn Progress",
					}},
				},
				WorkspacePath:   filepath.Join(t.TempDir(), "detent-647"),
				SessionID:       "session-647",
				ProcessIdentity: "4242",
				WorkerHost:      "worker-a",
			}},
		},
	}
	deps.Refresher = refresher
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := validOnboardingForm()
	form.Set("github_status_source", workflowconfig.GitHubStatusSourceLabel)
	form.Del("project_slug")
	form.Set("status_label_prefix", "detent:")
	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
	for _, want := range []string{
		"Wrote WORKFLOW.md",
		"Closeout verifier",
		"reload: observed project detent",
		"refresh: advanced",
		"candidate counts: expected status labels matched",
		"dispatch: started digitaldrywood/detent#647",
		"worktree: present",
		"Workpad: present",
		"/api/v1/refresh: reflected in project state",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestOnboardingWriteCloseoutVerifierReportsStalledReload(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	snapshots := hub.New[telemetry.Snapshot]()
	lastRefresh := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	if err := snapshots.Publish(telemetry.Snapshot{
		GeneratedAt: lastRefresh,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefresh,
			},
		}},
		Refresh: telemetry.Refresh{
			Status:        telemetry.RefreshStatusReady,
			LastRefreshAt: &lastRefresh,
		},
		Running: []telemetry.Running{{
			Issue: telemetry.Issue{
				ID:         "issue-active",
				Identifier: "digitaldrywood/detent#640",
				ProjectID:  "detent",
				State:      "In Progress",
			},
			WorkspacePath:   "/tmp/detent-640",
			SessionID:       "session-640",
			ProcessIdentity: "31337",
			WorkerHost:      "worker-b",
		}},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	deps := testDeps(t)
	deps.Hub = snapshots
	deps.Registry = project.NewRegistry()
	mustSetOnboardingProject(t, deps.Registry, "detent", workflowPath)
	deps.Refresher = &refreshProbe{
		response: web.RefreshResponse{
			Queued:      true,
			RequestedAt: lastRefresh.Add(30 * time.Second),
			Operations:  []string{"poll", "reconcile"},
		},
	}
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := validOnboardingForm()
	form.Set("github_status_source", workflowconfig.GitHubStatusSourceLabel)
	form.Del("project_slug")
	form.Set("status_label_prefix", "detent:")
	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, want := range []string{
		"Closeout verifier",
		"reload: stalled",
		"refresh: stalled",
		"dispatch: no new running issue observed",
		"active agents: digitaldrywood/detent#640 session=session-640 pid=31337 worker=worker-b worktree=/tmp/detent-640",
		"restart requires operator confirmation",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("body missing %q:\n%s", want, rec.Body.String())
		}
	}
}

func TestOnboardingWriteDoesNotOverwriteWithoutReplace(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("existing"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	rec := httptest.NewRecorder()
	req := onboardingRequest(http.MethodPost, "/onboarding/write", validOnboardingForm())

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Fatalf("body missing overwrite warning:\n%s", rec.Body.String())
	}
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(raw) != "existing" {
		t.Fatalf("workflow content = %q, want existing", raw)
	}

	form := validOnboardingForm()
	form.Set("replace", "true")
	rec = httptest.NewRecorder()
	req = onboardingRequest(http.MethodPost, "/onboarding/write", form)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	raw, err = os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("ReadFile() after replace error = %v", err)
	}
	if string(raw) == "existing" {
		t.Fatal("workflow was not replaced")
	}
}

func TestOnboardingWriteValidatesInput(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	tests := []struct {
		name string
		edit func(url.Values)
		want string
	}{
		{
			name: "missing tracker",
			edit: func(form url.Values) {
				form.Del("tracker_kind")
			},
			want: "tracker is required",
		},
		{
			name: "unsupported tracker",
			edit: func(form url.Values) {
				form.Set("tracker_kind", "linear")
			},
			want: "tracker must be github or memory",
		},
		{
			name: "invalid repo",
			edit: func(form url.Values) {
				form.Set("repo", "bad repo")
			},
			want: "repo must look like owner/name",
		},
		{
			name: "invalid concurrency",
			edit: func(form url.Values) {
				form.Set("max_concurrent_agents", "0")
			},
			want: "max concurrent agents must be positive",
		},
		{
			name: "polling interval below floor",
			edit: func(form url.Values) {
				form.Set("polling_interval_ms", "59999")
			},
			want: "polling interval must be at least 60000",
		},
		{
			name: "invalid kanban mode",
			edit: func(form url.Values) {
				form.Set("kanban_mode", "observer")
			},
			want: "kanban mode must be read_only or integration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			form := validOnboardingForm()
			tt.edit(form)

			rec := httptest.NewRecorder()
			req := onboardingRequest(http.MethodPost, "/onboarding/write", form)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body missing %q:\n%s", tt.want, rec.Body.String())
			}
			if _, err := os.Stat(workflowPath); !os.IsNotExist(err) {
				t.Fatalf("workflow file exists after validation error: %v", err)
			}
		})
	}
}

func validOnboardingForm() url.Values {
	return url.Values{
		"tracker_kind":                    {"github"},
		"endpoint":                        {"https://api.github.com/graphql"},
		"api_key":                         {"$GITHUB_TOKEN"},
		"project_slug":                    {"PVT_project"},
		"repo":                            {"digitaldrywood/detent"},
		"workspace_root":                  {"~/code/detent-workspaces"},
		"max_concurrent_agents":           {"5"},
		"max_turns":                       {"20"},
		"polling_interval_ms":             {"120000"},
		"merging_concurrency":             {"1"},
		"dispatch_priority_by_state":      {"Merging\nRework"},
		"dispatch_priority_by_label":      {"bug\nregression\nenhancement"},
		"dependency_auto_unblock_enabled": {"false"},
	}
}

func validMemoryOnboardingForm() url.Values {
	form := validOnboardingForm()
	form.Set("tracker_kind", "memory")
	form.Del("endpoint")
	form.Del("api_key")
	form.Del("project_slug")
	form.Del("repo")
	return form
}

func onboardingRequest(method string, path string, form url.Values) *http.Request {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
	}
	return req
}

func mustSetOnboardingProject(t *testing.T, registry *project.Registry, id string, workflowPath string) {
	t.Helper()

	workflowCfg := workflowconfig.Default()
	workflowCfg.Tracker.Kind = workflowconfig.TrackerMemory
	trackedProject, err := project.New(project.Config{
		Project: globalconfig.Project{
			ID:       id,
			Workflow: workflowPath,
		},
		Workflow: workflowconfig.Workflow{
			Config: workflowCfg,
			Prompt: "Work the issue.",
		},
	}, project.Dependencies{
		Connector: connectorProbe{name: "memory"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := registry.Set(trackedProject); err != nil {
		t.Fatalf("Registry.Set() error = %v", err)
	}
}

type publishSnapshotRefreshProbe struct {
	hub      *hub.Hub[telemetry.Snapshot]
	response web.RefreshResponse
	snapshot telemetry.Snapshot
	calls    int
}

func (p *publishSnapshotRefreshProbe) RequestRefresh(context.Context) (web.RefreshResponse, error) {
	p.calls++
	if err := p.hub.Publish(p.snapshot); err != nil {
		return web.RefreshResponse{}, err
	}
	return p.response, nil
}
