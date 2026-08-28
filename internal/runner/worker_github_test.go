package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/digitaldrywood/detent/internal/connector"
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
		resolveGH         workerGitHubTokenResolver
		wantEnabled       bool
		wantMode          workerGitHubCredentialMode
		wantToken         string
		wantErrText       string
	}{
		{name: "unset remains disabled", wantMode: workerGitHubCredentialDisabled},
		{name: "dedicated literal", workerToken: "worker-token", orchestratorToken: "orchestrator-token", wantEnabled: true, wantMode: workerGitHubCredentialUnclassified, wantToken: "worker-token"},
		{name: "dedicated environment reference", workerToken: "$WORKER_TOKEN", orchestratorToken: "orchestrator-token", environment: map[string]string{"WORKER_TOKEN": "worker-token"}, wantEnabled: true, wantMode: workerGitHubCredentialUnclassified, wantToken: "worker-token"},
		{name: "exact token classifies shared", workerToken: "$WORKER_TOKEN", orchestratorToken: "shared-token", environment: map[string]string{"WORKER_TOKEN": "shared-token"}, wantEnabled: true, wantMode: workerGitHubCredentialShared, wantToken: "shared-token"},
		{name: "gh sentinel resolves", workerToken: "gh", orchestratorToken: "ambient-token", resolveGH: func(context.Context) (string, error) { return " ambient-token\n", nil }, wantEnabled: true, wantMode: workerGitHubCredentialShared, wantToken: "ambient-token"},
		{name: "gh sentinel without orchestrator user token is distinct", workerToken: "gh", resolveGH: func(context.Context) (string, error) { return "ambient-token", nil }, wantEnabled: true, wantMode: workerGitHubCredentialDistinct, wantToken: "ambient-token"},
		{name: "gh sentinel with different orchestrator token needs principal classification", workerToken: "gh", orchestratorToken: "orchestrator-token", resolveGH: func(context.Context) (string, error) { return "ambient-token", nil }, wantEnabled: true, wantMode: workerGitHubCredentialUnclassified, wantToken: "ambient-token"},
		{name: "gh sentinel resolution failure", workerToken: "gh", resolveGH: func(context.Context) (string, error) { return "", errors.New("not logged in") }, wantErrText: "resolve worker.github_token via gh auth token: not logged in"},
		{name: "missing referenced credential", workerToken: "$WORKER_TOKEN", orchestratorToken: "orchestrator-token", wantErrText: "WORKER_TOKEN is empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Tracker.Kind = config.TrackerGitHub
			cfg.Tracker.Endpoint = "https://api.github.com/graphql"
			cfg.Tracker.APIKey = tt.orchestratorToken
			cfg.Worker.GitHubToken = tt.workerToken
			var logs bytes.Buffer
			policy, err := newWorkerGitHubPolicy(context.Background(), cfg, "detent", "digitaldrywood/detent#1724", func(key string) string {
				return tt.environment[key]
			}, tt.resolveGH, nil, slog.New(slog.NewTextHandler(&logs, nil)))
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
			if policy.CredentialMode != tt.wantMode {
				t.Fatalf("CredentialMode = %q, want %q", policy.CredentialMode, tt.wantMode)
			}
			if tt.wantMode == workerGitHubCredentialShared {
				policy.Principal.Login = "detent-worker[bot]"
				policy.PrincipalID = 42
				classified, classifyErr := policy.classifyCredential(context.Background())
				if classifyErr != nil {
					t.Fatalf("classifyCredential() error = %v", classifyErr)
				}
				policy = classified
				if !strings.Contains(logs.String(), "worker github credential uses shared REST budget") {
					t.Fatalf("logs = %q, want shared-budget warning", logs.String())
				}
			}
			if tt.wantEnabled && (!strings.HasPrefix(policy.CredentialIdentity, "github-rest:") || policy.Token != tt.wantToken) {
				t.Fatalf("policy = %#v, want redacted identity and resolved worker token", policy)
			}
			if tt.wantEnabled {
				probeCtx, cancel := policy.ProbeContext(context.Background())
				_, hasDeadline := probeCtx.Deadline()
				cancel()
				if !hasDeadline {
					t.Fatal("worker github probe context has no deadline")
				}
			}
		})
	}
}

func TestNewWorkerGitHubPolicyUsesGitHubEndpointForNonGitHubTracker(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Tracker.Kind = config.TrackerLinear
	cfg.Tracker.Endpoint = "https://api.linear.app/graphql"
	cfg.Tracker.APIKey = "linear-secret"
	cfg.Worker.GitHubToken = "worker-token"
	policy, err := newWorkerGitHubPolicy(context.Background(), cfg, "detent", "digitaldrywood/detent#1724", func(string) string { return "" }, nil, nil, nil)
	if err != nil {
		t.Fatalf("newWorkerGitHubPolicy() error = %v", err)
	}
	if policy.GraphQLURL != "https://api.github.com/graphql" {
		t.Fatalf("GraphQLURL = %q, want public GitHub API", policy.GraphQLURL)
	}
	if policy.RateLimitURL != "https://api.github.com/rate_limit" {
		t.Fatalf("RateLimitURL = %q, want public GitHub API", policy.RateLimitURL)
	}
	if policy.OrchestratorToken != "" {
		t.Fatalf("OrchestratorToken = %q, want non-GitHub tracker credential excluded", policy.OrchestratorToken)
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
		{name: "resolved ambient token overrides ambient tokens", token: "ambient-token"},
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

func TestWorkerGitHubSentinelResolvesAndInjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sentinel string
	}{
		{name: "gh", sentinel: "gh"},
		{name: "gh auth alias", sentinel: "${gh auth token}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Worker.GitHubToken = tt.sentinel
			policy, err := newWorkerGitHubPolicy(context.Background(), cfg, "detent", "digitaldrywood/detent#1796", nil, func(context.Context) (string, error) {
				return "resolved-token", nil
			}, nil, nil)
			if err != nil {
				t.Fatalf("newWorkerGitHubPolicy() error = %v", err)
			}
			request := AgentTurnRequest{
				TempDir: t.TempDir(),
				Environment: procgroup.Environment{Variables: map[string]string{
					"GH_CONFIG_DIR": "/ambient/config",
					"GH_TOKEN":      "ambient-token",
				}},
				workerGitHub: policy,
			}
			if err := configureWorkerGitHubEnvironment(&request); err != nil {
				t.Fatalf("configureWorkerGitHubEnvironment() error = %v", err)
			}
			if request.Environment.Variables["GH_TOKEN"] != "resolved-token" || request.Environment.Variables["GITHUB_TOKEN"] != "resolved-token" {
				t.Fatalf("worker token environment = %#v, want resolved token", request.Environment.Variables)
			}
			if request.Environment.Variables["GH_CONFIG_DIR"] == "/ambient/config" {
				t.Fatal("GH_CONFIG_DIR preserved ambient GitHub CLI configuration")
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

func TestWorkerGitHubGovernorPublishesBudgetAttribution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		mode            workerGitHubCredentialMode
		reserve         int64
		wantConsumer    string
		wantFamily      string
		wantAttribution string
	}{
		{name: "distinct principal attributes worker pool", mode: workerGitHubCredentialDistinct, reserve: 1000, wantConsumer: telemetry.RESTConsumerWorker, wantFamily: "worker credential", wantAttribution: "worker"},
		{name: "shared principal leaves pool attribution indeterminate", mode: workerGitHubCredentialShared, reserve: 1250, wantConsumer: telemetry.RESTConsumerSharedPool, wantFamily: "shared credential pool", wantAttribution: "indeterminate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := workerGitHubRateLimitServer(t, func(int64) int64 { return 4200 })
			t.Cleanup(server.Close)
			var logs bytes.Buffer
			policy := workerGitHubTestPolicy(server, &logs)
			policy.CredentialMode = tt.mode
			policy.MinRemaining = tt.reserve
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
				t.Fatalf("RateLimits = %#v, want one governed budget", update.RateLimits)
			}
			budget := update.RateLimits.GitHubRESTBudgets[0]
			if budget.Consumer != tt.wantConsumer || budget.EndpointFamily != tt.wantFamily || budget.Remaining != 4200 || budget.MinRemainingReserve != tt.reserve {
				t.Fatalf("budget = %#v, want consumer %q family %q reserve %d", budget, tt.wantConsumer, tt.wantFamily, tt.reserve)
			}
			for _, want := range []string{"consumer=" + tt.wantConsumer, "remaining=4200", "reserve=" + strconv.FormatInt(tt.reserve, 10), "credential_identity=github-rest:", "usage_attribution=" + tt.wantAttribution} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("logs = %q, want containing %q", logs.String(), want)
				}
			}
		})
	}
}

func TestWorkerGitHubGovernorStopsAtReserve(t *testing.T) {
	t.Parallel()

	server := workerGitHubRateLimitServer(t, func(call int64) int64 {
		if call == 1 {
			return 1300
		}
		return 1200
	})
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	poll := make(chan time.Time, 1)
	policy := workerGitHubTestPolicy(server, &logs)
	policy.CredentialMode = workerGitHubCredentialShared
	policy.MinRemaining = 1250
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
	for _, want := range []string{"consumer=shared_pool", "credential_identity=github-rest:worker"} {
		if !strings.Contains(context.Cause(governedCtx).Error(), want) {
			t.Fatalf("context cause = %q, want containing %q", context.Cause(governedCtx), want)
		}
	}
	for _, want := range []string{"remaining=1200", "worker_reserved_headroom=1250", "orchestrator_dispatch_floor=1000"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs = %q, want containing %q", logs.String(), want)
		}
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
	if !strings.Contains(logs.String(), "worker_reserved_headroom=1000") {
		t.Fatalf("logs = %q, want reserved headroom warning", logs.String())
	}
}

func TestWorkerGitHubCredentialPrincipalClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		orchestratorUserID int64
		wantMode           workerGitHubCredentialMode
		wantWarning        bool
	}{
		{name: "same principal uses shared budget", orchestratorUserID: 42, wantMode: workerGitHubCredentialShared, wantWarning: true},
		{name: "distinct principal remains isolated", orchestratorUserID: 84, wantMode: workerGitHubCredentialDistinct},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := workerGitHubPrincipalServer(t, tt.orchestratorUserID)
			t.Cleanup(server.Close)
			var logs bytes.Buffer
			policy := workerGitHubTestPolicy(server, &logs)
			policy.CredentialMode = workerGitHubCredentialUnclassified
			policy.OrchestratorToken = "orchestrator-token"
			policy.MinRemaining = 1250
			classified, err := policy.classifyCredential(context.Background())
			if err != nil {
				t.Fatalf("classifyCredential() error = %v", err)
			}
			if classified.CredentialMode != tt.wantMode {
				t.Fatalf("CredentialMode = %q, want %q", classified.CredentialMode, tt.wantMode)
			}
			if classified.Principal.Login != "detent-worker[bot]" {
				t.Fatalf("Principal = %#v, want Detent worker bot", classified.Principal)
			}
			warning := strings.Contains(logs.String(), "worker github credential uses shared REST budget")
			if warning != tt.wantWarning {
				t.Fatalf("shared-budget warning present = %t, want %t: %q", warning, tt.wantWarning, logs.String())
			}
		})
	}
}

func TestWorkerGitHubSharedReserveValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      workerGitHubCredentialMode
		reserve   int64
		wantError bool
	}{
		{name: "shared reserve above floor", mode: workerGitHubCredentialShared, reserve: 1250},
		{name: "shared reserve equal to floor", mode: workerGitHubCredentialShared, reserve: 1000, wantError: true},
		{name: "distinct reserve equal to floor remains valid", mode: workerGitHubCredentialDistinct, reserve: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := workerGitHubPolicy{
				Enabled:           true,
				CredentialMode:    tt.mode,
				PrincipalID:       42,
				MinRemaining:      tt.reserve,
				OrchestratorFloor: 1000,
			}
			policy.Principal.Login = "detent-worker[bot]"
			_, err := policy.classifyCredential(context.Background())
			if got := errors.Is(err, ErrWorkerGitHubSharedReserve); got != tt.wantError {
				t.Fatalf("classifyCredential() error = %v, shared reserve error=%t, want %t", err, got, tt.wantError)
			}
		})
	}
}

func TestWorkerGitHubGovernorBoundsStalledProbe(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{}, 1)
	cancelProbe := make(chan context.CancelFunc, 1)
	policy := workerGitHubPolicy{
		Enabled:            true,
		CredentialMode:     workerGitHubCredentialDistinct,
		Token:              "worker-token",
		CredentialIdentity: "github-rest:worker",
		GraphQLURL:         "https://api.github.test/graphql",
		RateLimitURL:       "https://api.github.test/rate_limit",
		HTTPClient: workerGitHubHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
			requestStarted <- struct{}{}
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		ProbeContext: func(ctx context.Context) (context.Context, context.CancelFunc) {
			probeCtx, cancel := context.WithCancel(ctx)
			cancelProbe <- cancel
			return probeCtx, cancel
		},
	}
	preclassifyWorkerGitHubPolicy(&policy)
	result := make(chan error, 1)
	go func() {
		_, _, err := startWorkerGitHubGovernor(context.Background(), policy, nil)
		result <- err
	}()

	var cancel context.CancelFunc
	select {
	case cancel = <-cancelProbe:
	case <-time.After(2 * time.Second):
		t.Fatal("probe context was not created")
	}
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("probe request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrWorkerGitHubBudgetMonitor) || !errors.Is(err, context.Canceled) {
			t.Fatalf("startWorkerGitHubGovernor() error = %v, want bounded canceled probe", err)
		}
		monitorErr, ok := AsWorkerGitHubBudgetMonitorError(err)
		if !ok || monitorErr.Operation != "launch_probe" || monitorErr.CredentialIdentity == "" {
			t.Fatalf("monitor error = %#v, want credential-scoped launch probe", monitorErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("governor did not return after the probe context was canceled")
	}
}

func TestWorkerGitHubMonitorErrorsCarryCredentialScope(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("github transport unavailable")
	server := workerGitHubRateLimitServer(t, func(int64) int64 { return 4200 })
	t.Cleanup(server.Close)
	tests := []struct {
		name          string
		wantOperation string
		run           func() error
	}{
		{
			name:          "launch probe",
			wantOperation: "launch_probe",
			run: func() error {
				policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
				preclassifyWorkerGitHubPolicy(&policy)
				policy.HTTPClient = workerGitHubHTTPClientFunc(func(*http.Request) (*http.Response, error) {
					return nil, transportErr
				})
				_, _, err := startWorkerGitHubGovernor(t.Context(), policy, nil)
				return err
			},
		},
		{
			name:          "launch observation",
			wantOperation: "launch_observation",
			run: func() error {
				policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
				preclassifyWorkerGitHubPolicy(&policy)
				_, _, err := startWorkerGitHubGovernor(t.Context(), policy, func(AgentUpdate) error {
					return transportErr
				})
				return err
			},
		},
		{
			name:          "periodic probe",
			wantOperation: "periodic_probe",
			run: func() error {
				poll := make(chan time.Time, 1)
				var calls atomic.Int64
				policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
				preclassifyWorkerGitHubPolicy(&policy)
				policy.Poll = poll
				policy.HTTPClient = workerGitHubHTTPClientFunc(func(*http.Request) (*http.Response, error) {
					if calls.Add(1) > 1 {
						return nil, transportErr
					}
					return workerGitHubRateLimitResponse(), nil
				})
				governedCtx, stop, err := startWorkerGitHubGovernor(t.Context(), policy, nil)
				if err != nil {
					return err
				}
				poll <- time.Now()
				<-governedCtx.Done()
				return stop()
			},
		},
		{
			name:          "periodic observation",
			wantOperation: "periodic_observation",
			run: func() error {
				poll := make(chan time.Time, 1)
				var updates atomic.Int64
				policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
				preclassifyWorkerGitHubPolicy(&policy)
				policy.Poll = poll
				governedCtx, stop, err := startWorkerGitHubGovernor(t.Context(), policy, func(AgentUpdate) error {
					if updates.Add(1) > 1 {
						return transportErr
					}
					return nil
				})
				if err != nil {
					return err
				}
				poll <- time.Now()
				<-governedCtx.Done()
				return stop()
			},
		},
		{
			name:          "worker credential classification",
			wantOperation: "credential_classification_worker",
			run: func() error {
				policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
				policy.CredentialMode = workerGitHubCredentialUnclassified
				policy.HTTPClient = workerGitHubHTTPClientFunc(func(*http.Request) (*http.Response, error) {
					return nil, transportErr
				})
				_, _, err := startWorkerGitHubGovernor(t.Context(), policy, nil)
				return err
			},
		},
		{
			name:          "orchestrator credential classification",
			wantOperation: "credential_classification_orchestrator",
			run: func() error {
				policy := workerGitHubTestPolicy(server, new(bytes.Buffer))
				policy.CredentialMode = workerGitHubCredentialUnclassified
				policy.OrchestratorToken = "orchestrator-token"
				policy.HTTPClient = workerGitHubHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
					if request.Header.Get("Authorization") == "Bearer orchestrator-token" {
						return nil, transportErr
					}
					return workerGitHubPrincipalResponse(), nil
				})
				_, _, err := startWorkerGitHubGovernor(t.Context(), policy, nil)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.run()
			if !errors.Is(err, ErrWorkerGitHubBudgetMonitor) || !errors.Is(err, transportErr) {
				t.Fatalf("error = %v, want monitor and transport errors", err)
			}
			monitorErr, ok := AsWorkerGitHubBudgetMonitorError(err)
			if !ok {
				t.Fatalf("error = %T %v, want WorkerGitHubBudgetMonitorError", err, err)
			}
			if monitorErr.Operation != tt.wantOperation || monitorErr.CredentialIdentity != "github-rest:worker" || monitorErr.Consumer != telemetry.RESTConsumerWorker {
				t.Fatalf("monitor error = %#v, want operation %q and worker credential scope", monitorErr, tt.wantOperation)
			}
		})
	}
}

func TestWorkerGitHubPeriodicMonitorFailureThroughSupervisor(t *testing.T) {
	t.Parallel()

	poll := make(chan time.Time, 1)
	var calls atomic.Int64
	policy := workerGitHubPolicy{
		Enabled:            true,
		CredentialMode:     workerGitHubCredentialDistinct,
		Token:              "worker-token",
		CredentialIdentity: "github-rest:worker",
		RateLimitURL:       "https://api.github.test/rate_limit",
		GraphQLURL:         "https://api.github.test/graphql",
		MinRemaining:       1000,
		OrchestratorFloor:  1000,
		Poll:               poll,
		HTTPClient: workerGitHubHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) > 1 {
				return nil, errors.New("periodic DNS failure")
			}
			return workerGitHubRateLimitResponse(), nil
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	preclassifyWorkerGitHubPolicy(&policy)
	backend := workerGitHubGovernorBackend{
		policy: policy,
		poll:   poll,
		result: RunResult{FinalState: FinalStateFailed, TurnStarted: true, Tokens: TokenTotals{InputTokens: 2_000_000, OutputTokens: 1_000_000, TotalTokens: 3_000_000}},
	}
	supervisor, err := NewSupervisor(backend, SupervisorConfig{})
	if err != nil {
		t.Fatalf("NewSupervisor() error = %v", err)
	}
	completion := supervisor.Run(t.Context(), RunRequest{Issue: connector.Issue{ID: "issue-2028"}, Attempt: 4})

	monitorErr, ok := AsWorkerGitHubBudgetMonitorError(completion.Err)
	if !ok || monitorErr.Operation != "periodic_probe" {
		t.Fatalf("completion error = %#v, want periodic monitor failure", completion.Err)
	}
	if completion.Retryable || completion.RetryAttempt != 0 || completion.RetryDelay != 0 {
		t.Fatalf("generic retry state = %v, %d, %s; want cooperative monitor stop", completion.Retryable, completion.RetryAttempt, completion.RetryDelay)
	}
	if completion.Result.Tokens.TotalTokens != 3_000_000 {
		t.Fatalf("tokens = %#v, want actual usage intact", completion.Result.Tokens)
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
			_, _ = w.Write([]byte(`{"data":{"viewer":{"databaseId":42,"login":"detent-worker[bot]","__typename":"Bot"}}}`))
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

func workerGitHubPrincipalServer(t *testing.T, orchestratorUserID int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Errorf("path = %q, want /graphql", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		userID := int64(42)
		login := "detent-worker[bot]"
		if r.Header.Get("Authorization") == "Bearer orchestrator-token" {
			userID = orchestratorUserID
			login = "detent-orchestrator[bot]"
			if userID == 42 {
				login = "detent-worker[bot]"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"databaseId":%d,"login":%q,"__typename":"Bot"}}}`, userID, login)
	}))
}

func workerGitHubTestPolicy(server *httptest.Server, logs *bytes.Buffer) workerGitHubPolicy {
	return workerGitHubPolicy{
		Enabled:            true,
		CredentialMode:     workerGitHubCredentialDistinct,
		Token:              "worker-token",
		CredentialIdentity: "github-rest:worker",
		RateLimitURL:       server.URL + "/rate_limit",
		GraphQLURL:         server.URL + "/graphql",
		MinRemaining:       1000,
		OrchestratorFloor:  1000,
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

type workerGitHubGovernorBackend struct {
	policy workerGitHubPolicy
	poll   chan<- time.Time
	result RunResult
}

type workerGitHubHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f workerGitHubHTTPClientFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (b workerGitHubGovernorBackend) Run(ctx context.Context, _ RunRequest) (RunResult, error) {
	governedCtx, stop, err := startWorkerGitHubGovernor(ctx, b.policy, nil)
	if err != nil {
		return b.result, err
	}
	b.poll <- time.Now()
	<-governedCtx.Done()
	cause := context.Cause(governedCtx)
	_ = stop()
	return b.result, cause
}

func workerGitHubRateLimitResponse() *http.Response {
	resetAt := time.Now().Add(time.Hour).Unix()
	body := fmt.Sprintf(`{"resources":{"core":{"limit":5000,"used":800,"remaining":4200,"reset":%d}}}`, resetAt)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func workerGitHubPrincipalResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"data":{"viewer":{"databaseId":42,"login":"detent-worker[bot]","__typename":"Bot"}}}`)),
	}
}

func preclassifyWorkerGitHubPolicy(policy *workerGitHubPolicy) {
	policy.PrincipalID = 42
	policy.Principal.Login = "detent-worker[bot]"
}

func (b *workerGitHubCaptureBackend) RunTurn(_ context.Context, request AgentTurnRequest, _ AgentUpdateHandler) (AgentTurnResult, error) {
	b.request = request
	return AgentTurnResult{}, nil
}
