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

	"github.com/digitaldrywood/symphony-go/internal/web"
)

func TestOnboardingRoutesProgressThroughWizard(t *testing.T) {
	t.Parallel()

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server, err := web.NewServer(web.Config{WorkflowPath: workflowPath}, testDeps(t))
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
				"Symphony onboarding",
				"Choose tracker",
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
				"repo":         {"digitaldrywood/symphony-go"},
			},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Agent config",
				"name=\"max_concurrent_agents\"",
				"name=\"workspace_root\"",
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
				"repo":                       {"digitaldrywood/symphony-go"},
				"workspace_root":             {"~/code/symphony-workspaces"},
				"max_concurrent_agents":      {"5"},
				"max_turns":                  {"20"},
				"polling_interval_ms":        {"30000"},
				"merging_concurrency":        {"1"},
				"dispatch_priority_by_state": {"Merging\nRework"},
			},
			wantStatus: http.StatusOK,
			wantContent: []string{
				"Write WORKFLOW.md",
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
	for _, want := range []string{
		"tracker:\n  kind: github",
		"api_key: $GITHUB_TOKEN",
		"project_slug: PVT_project",
		"git clone --depth 1 https://github.com/digitaldrywood/symphony-go .",
		"max_concurrent_agents_by_state:\n    Merging: 1",
		"You are working on GitHub issue `{{ issue.identifier }}`",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("workflow missing %q:\n%s", want, content)
		}
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
		"tracker_kind":               {"github"},
		"endpoint":                   {"https://api.github.com/graphql"},
		"api_key":                    {"$GITHUB_TOKEN"},
		"project_slug":               {"PVT_project"},
		"repo":                       {"digitaldrywood/symphony-go"},
		"workspace_root":             {"~/code/symphony-workspaces"},
		"max_concurrent_agents":      {"5"},
		"max_turns":                  {"20"},
		"polling_interval_ms":        {"30000"},
		"merging_concurrency":        {"1"},
		"dispatch_priority_by_state": {"Merging\nRework"},
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
