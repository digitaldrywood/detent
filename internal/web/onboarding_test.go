package web_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
				"hx-post=\"/onboarding/tracker\"",
			},
		},
		{
			name:       "tracker to credentials",
			method:     http.MethodPost,
			path:       "/onboarding/tracker",
			form:       url.Values{"tracker_kind": {"github"}},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Credentials",
				onboardingStepBadge("credentials"),
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

func onboardingStepBadge(step string) string {
	return `data-onboarding-step-badge="true"><span class="text-xs font-medium uppercase text-muted-foreground">Step</span> <span class="truncate font-mono text-xs font-semibold text-foreground">` + step + `</span>`
}

func TestOnboardingWriteWorkflow(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	form := validOnboardingForm()
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
	for _, want := range []string{
		"tracker:\n  kind: github",
		"api_key: $GITHUB_TOKEN",
		"project_slug: PVT_project",
		"dependency_auto_unblock:\n    enabled: false\n    source_states:\n      - Blocked\n    target_state: Todo\n    readiness: terminal_or_merged",
		"source_root: " + sourceRoot,
		"codex:\n  command: codex app-server",
		"gate:\n  kind: command\n  run: make check",
		"hooks:\n  timeout_ms: 60000",
		"max_concurrent_agents_by_state:\n    Merging: 1",
		"dispatch_priority_by_label:\n    - bug\n    - regression\n    - enhancement",
		"You are working on GitHub issue `{{ issue.identifier }}`",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("workflow missing %q:\n%s", want, content)
		}
	}
	for _, unwanted := range []string{
		"codex:\n  command: codex app-server\n  shell:",
		"hooks:\n  shell:",
		"after_create:",
		"git clone",
		"git worktree add",
	} {
		if strings.Contains(content, unwanted) {
			t.Fatalf("workflow contains %q:\n%s", unwanted, content)
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
