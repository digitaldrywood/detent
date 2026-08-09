package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestNewWorkerGitHubPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		workerToken       string
		orchestratorToken string
		environment       map[string]string
		wantEnabled       bool
		wantErr           error
		wantErrText       string
	}{
		{name: "disabled isolates ambient authentication"},
		{name: "dedicated literal", workerToken: "worker-token", orchestratorToken: "orchestrator-token", wantEnabled: true},
		{name: "dedicated environment reference", workerToken: "$WORKER_TOKEN", orchestratorToken: "orchestrator-token", environment: map[string]string{"WORKER_TOKEN": "worker-token"}, wantEnabled: true},
		{name: "shared credential rejected", workerToken: "$WORKER_TOKEN", orchestratorToken: "shared-token", environment: map[string]string{"WORKER_TOKEN": "shared-token"}, wantErr: ErrWorkerGitHubCredentialShared},
		{name: "missing referenced credential", workerToken: "$WORKER_TOKEN", orchestratorToken: "orchestrator-token", wantErrText: "WORKER_TOKEN is empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Tracker.Endpoint = "https://api.github.com/graphql"
			cfg.Tracker.APIKey = tt.orchestratorToken
			cfg.Worker.GitHubToken = tt.workerToken
			policy, err := newWorkerGitHubPolicy(cfg, "detent", "digitaldrywood/detent#1724", func(key string) string {
				return tt.environment[key]
			}, nil, nil)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("newWorkerGitHubPolicy() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("newWorkerGitHubPolicy() error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("newWorkerGitHubPolicy() error = %v", err)
			}
			if policy.Enabled != tt.wantEnabled {
				t.Fatalf("Enabled = %t, want %t", policy.Enabled, tt.wantEnabled)
			}
			if tt.wantEnabled && (!strings.HasPrefix(policy.CredentialIdentity, "github-rest:") || policy.Token != "worker-token") {
				t.Fatalf("policy = %#v, want redacted identity and resolved worker token", policy)
			}
		})
	}
}

func TestConfigureWorkerGitHubEnvironment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
	}{
		{name: "disabled clears ambient tokens"},
		{name: "dedicated token overrides ambient tokens", token: "worker-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tempDir := t.TempDir()
			request := AgentTurnRequest{
				TempDir: tempDir,
				Environment: procgroup.Environment{Variables: map[string]string{
					"GH_TOKEN":                "ambient-gh",
					"GITHUB_TOKEN":            "ambient-github",
					"GH_ENTERPRISE_TOKEN":     "ambient-gh-enterprise",
					"GITHUB_ENTERPRISE_TOKEN": "ambient-github-enterprise",
					"EXISTING":                "value",
				}},
				workerGitHub: workerGitHubPolicy{Token: tt.token},
			}
			if err := configureWorkerGitHubEnvironment(&request); err != nil {
				t.Fatalf("configureWorkerGitHubEnvironment() error = %v", err)
			}
			if request.Environment.Variables["GH_TOKEN"] != tt.token || request.Environment.Variables["GITHUB_TOKEN"] != tt.token {
				t.Fatalf("token environment = %#v, want %q", request.Environment.Variables, tt.token)
			}
			if request.Environment.Variables["GH_ENTERPRISE_TOKEN"] != tt.token || request.Environment.Variables["GITHUB_ENTERPRISE_TOKEN"] != tt.token {
				t.Fatalf("enterprise token environment = %#v, want %q", request.Environment.Variables, tt.token)
			}
			if request.Environment.Variables["EXISTING"] != "value" {
				t.Fatalf("EXISTING = %q, want value", request.Environment.Variables["EXISTING"])
			}
			configDir := filepath.Join(tempDir, "github-cli")
			if request.Environment.Variables["GH_CONFIG_DIR"] != configDir {
				t.Fatalf("GH_CONFIG_DIR = %q, want %q", request.Environment.Variables["GH_CONFIG_DIR"], configDir)
			}
			if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
				t.Fatalf("worker github config directory = %v, %v", info, err)
			}
		})
	}
}

func TestRunAgentBackendTurnAppliesWorkerGitHubPolicy(t *testing.T) {
	t.Parallel()

	server := workerGitHubRateLimitServer(t, func(int64) int64 { return 4200 })
	t.Cleanup(server.Close)
	tests := []struct {
		name   string
		policy workerGitHubPolicy
		token  string
	}{
		{name: "disabled"},
		{name: "dedicated", policy: workerGitHubTestPolicy(server, new(bytes.Buffer)), token: "worker-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend := &workerGitHubCaptureBackend{}
			_, runErr, cleanupErr := runAgentBackendTurn(context.Background(), backend, AgentTurnRequest{
				Workspace: t.TempDir(),
				Environment: procgroup.Environment{Variables: map[string]string{
					"GH_TOKEN":     "ambient-gh",
					"GITHUB_TOKEN": "ambient-github",
				}},
				workerGitHub: tt.policy,
			}, nil)
			if runErr != nil || cleanupErr != nil {
				t.Fatalf("runAgentBackendTurn() errors = %v, %v", runErr, cleanupErr)
			}
			variables := backend.request.Environment.Variables
			if variables["GH_TOKEN"] != tt.token || variables["GITHUB_TOKEN"] != tt.token {
				t.Fatalf("worker token environment = %#v, want %q", variables, tt.token)
			}
			if variables["GH_ENTERPRISE_TOKEN"] != tt.token || variables["GITHUB_ENTERPRISE_TOKEN"] != tt.token {
				t.Fatalf("worker enterprise token environment = %#v, want %q", variables, tt.token)
			}
			if !strings.Contains(variables["GH_CONFIG_DIR"], filepath.Join(".detent", "tmp")) {
				t.Fatalf("GH_CONFIG_DIR = %q, want isolated worker scratch", variables["GH_CONFIG_DIR"])
			}
		})
	}
}

func TestWorkerGitHubGovernorPublishesAttributedBudget(t *testing.T) {
	t.Parallel()

	server := workerGitHubRateLimitServer(t, func(int64) int64 { return 4200 })
	defer server.Close()
	var logs bytes.Buffer
	policy := workerGitHubTestPolicy(server, &logs)
	var update AgentUpdate
	governedCtx, stop, err := startWorkerGitHubGovernor(context.Background(), policy, func(got AgentUpdate) error {
		update = got
		return nil
	})
	if err != nil {
		t.Fatalf("startWorkerGitHubGovernor() error = %v", err)
	}
	if governedCtx == nil {
		t.Fatal("governed context is nil")
	}
	if err := stop(); err != nil {
		t.Fatalf("stop governor error = %v", err)
	}
	if update.RateLimits == nil || len(update.RateLimits.GitHubRESTBudgets) != 1 {
		t.Fatalf("RateLimits = %#v, want worker budget", update.RateLimits)
	}
	budget := update.RateLimits.GitHubRESTBudgets[0]
	if budget.Consumer != telemetry.RESTConsumerWorker || budget.Remaining != 4200 || budget.MinRemainingReserve != 1000 {
		t.Fatalf("budget = %#v, want attributed governed worker budget", budget)
	}
	for _, want := range []string{"consumer=worker", "remaining=4200", "reserve=1000", "credential_identity=github-rest:"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs = %q, want containing %q", logs.String(), want)
		}
	}
}

func TestWorkerGitHubGovernorStopsAtReserve(t *testing.T) {
	t.Parallel()

	server := workerGitHubRateLimitServer(t, func(call int64) int64 {
		if call == 1 {
			return 1200
		}
		return 1000
	})
	defer server.Close()
	var logs bytes.Buffer
	poll := make(chan time.Time, 1)
	policy := workerGitHubTestPolicy(server, &logs)
	policy.Poll = poll
	governedCtx, stop, err := startWorkerGitHubGovernor(context.Background(), policy, nil)
	if err != nil {
		t.Fatalf("startWorkerGitHubGovernor() error = %v", err)
	}
	poll <- time.Now()
	<-governedCtx.Done()
	if !errors.Is(context.Cause(governedCtx), ErrWorkerGitHubRESTReserved) {
		t.Fatalf("context cause = %v, want ErrWorkerGitHubRESTReserved", context.Cause(governedCtx))
	}
	if err := stop(); !errors.Is(err, ErrWorkerGitHubRESTReserved) {
		t.Fatalf("stop governor error = %v, want ErrWorkerGitHubRESTReserved", err)
	}
	if !strings.Contains(logs.String(), "orchestrator_reserved_headroom=1000") {
		t.Fatalf("logs = %q, want reserved headroom warning", logs.String())
	}
}

func TestWorkerGitHubGovernorRefusesLaunchAtReserve(t *testing.T) {
	t.Parallel()

	server := workerGitHubRateLimitServer(t, func(int64) int64 { return 1000 })
	defer server.Close()
	var logs bytes.Buffer
	policy := workerGitHubTestPolicy(server, &logs)
	_, _, err := startWorkerGitHubGovernor(context.Background(), policy, nil)
	if !errors.Is(err, ErrWorkerGitHubRESTReserved) {
		t.Fatalf("startWorkerGitHubGovernor() error = %v, want ErrWorkerGitHubRESTReserved", err)
	}
	if !strings.Contains(logs.String(), "orchestrator_reserved_headroom=1000") {
		t.Fatalf("logs = %q, want reserved headroom warning", logs.String())
	}
}

func TestWorkerGitHubGovernorRejectsTokensForSameUser(t *testing.T) {
	t.Parallel()

	server := workerGitHubRateLimitServer(t, func(int64) int64 { return 4200 })
	defer server.Close()
	policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
	policy.OrchestratorToken = "orchestrator-token"
	_, _, err := startWorkerGitHubGovernor(context.Background(), policy, nil)
	if !errors.Is(err, ErrWorkerGitHubCredentialShared) {
		t.Fatalf("startWorkerGitHubGovernor() error = %v, want ErrWorkerGitHubCredentialShared", err)
	}
}

func TestMergeAgentRateLimitsPreservesWorkerAndProviderBudgets(t *testing.T) {
	t.Parallel()

	worker := &telemetry.RateLimits{GitHubRESTBudgets: []telemetry.RESTBudget{{Consumer: telemetry.RESTConsumerWorker, CredentialIdentity: "worker", Remaining: 4000}}}
	provider := &telemetry.RateLimits{LimitID: "codex", Primary: &telemetry.RateLimitBucket{Remaining: 50}}
	merged := mergeAgentRateLimits(worker, provider)
	if merged.LimitID != "codex" || merged.Primary == nil || merged.Primary.Remaining != 50 {
		t.Fatalf("merged provider rate limits = %#v", merged)
	}
	if len(merged.GitHubRESTBudgets) != 1 || merged.GitHubRESTBudgets[0].Consumer != telemetry.RESTConsumerWorker {
		t.Fatalf("merged worker budgets = %#v", merged.GitHubRESTBudgets)
	}
}

func workerGitHubRateLimitServer(t *testing.T, remaining func(int64) int64) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"viewer":{"databaseId":42}}}`))
		case "/rate_limit":
			if r.Header.Get("Authorization") != "Bearer worker-token" {
				t.Errorf("Authorization = %q, want worker bearer token", r.Header.Get("Authorization"))
			}
			value := remaining(calls.Add(1))
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"resources":{"core":{"limit":5000,"used":%s,"remaining":%s,"reset":%s}}}`,
				strconv.FormatInt(5000-value, 10),
				strconv.FormatInt(value, 10),
				strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
			)
		default:
			t.Errorf("path = %q, want /graphql or /rate_limit", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func workerGitHubTestPolicy(server *httptest.Server, logs *bytes.Buffer) workerGitHubPolicy {
	return workerGitHubPolicy{
		Enabled:            true,
		Token:              "worker-token",
		CredentialIdentity: "github-rest:worker",
		RateLimitURL:       server.URL + "/rate_limit",
		GraphQLURL:         server.URL + "/graphql",
		MinRemaining:       1000,
		PollInterval:       time.Hour,
		HTTPClient:         server.Client(),
		Logger:             slog.New(slog.NewTextHandler(logs, nil)),
		ProjectID:          "detent",
		IssueIdentifier:    "digitaldrywood/detent#1724",
	}
}

type workerGitHubCaptureBackend struct {
	request AgentTurnRequest
}

func (b *workerGitHubCaptureBackend) RunTurn(_ context.Context, request AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.request = request
	return AgentTurnResult{}, nil
}
