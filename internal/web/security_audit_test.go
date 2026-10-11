package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestSecurityAuditDispositionAcceptsProjectScopedWorkerCredential(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	mustSetWebProject(t, deps.Registry, "detent", false)
	key := securityaudit.Key{
		ProjectID:  "detent",
		Repository: "digitaldrywood/detent",
		PRNumber:   2006,
		BaseSHA:    "base-7",
		HeadSHA:    "head-7",
	}
	run, err := deps.Store.RecordSecurityAuditRun(t.Context(), securityaudit.Run{
		InvocationID:       "invocation-7",
		ProjectID:          key.ProjectID,
		IssueID:            "issue-2005",
		Identifier:         "digitaldrywood/detent#2005",
		IssueURL:           "https://github.com/digitaldrywood/detent/issues/2005",
		Repository:         key.Repository,
		PRNumber:           key.PRNumber,
		BaseSHA:            key.BaseSHA,
		HeadSHA:            key.HeadSHA,
		ServiceIdentity:    "detent:detent",
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: securityaudit.AuthenticationSubscription,
		WorkerPID:          4200,
		WorkerPGID:         4200,
		WorkerStartedAt:    now.Add(time.Second),
		ProviderThreadID:   "thread-7",
		ProviderSessionID:  "session-7",
		ExitStatus:         securityaudit.ExitStatusSuccess,
		OutputDigest:       securityaudit.OutputDigest(`{"verdict":"fail"}`),
		OutputBytes:        18,
		Verdict:            securityaudit.VerdictFail,
		Summary:            "One actionable finding.",
		Findings: []securityaudit.Finding{
			{ID: "auth-1", Severity: "p2", Body: "Authorization is missing."},
			{ID: "auth-2", Severity: "p2", Body: "Project scope is missing."},
		},
		Attempt:     1,
		StartedAt:   now,
		CompletedAt: now.Add(2 * time.Second),
		RecordedAt:  now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("RecordSecurityAuditRun() error = %v", err)
	}
	workerCredentials, err := serviceapi.NewWorkerCredentials()
	if err != nil {
		t.Fatalf("serviceapi.NewWorkerCredentials() error = %v", err)
	}
	deps.WorkerCredentials = workerCredentials
	global := globalconfig.Config{APIToken: "old-operator-token"}
	server, err := web.NewServer(web.Config{
		GlobalConfig:       global,
		GlobalConfigSource: func() globalconfig.Config { return global },
		Now:                func() time.Time { return now.Add(3 * time.Second) },
	}, deps)
	if err != nil {
		t.Fatalf("web.NewServer() error = %v", err)
	}
	form := url.Values{
		"repository":       {key.Repository},
		"pull_request":     {"2006"},
		"base_sha":         {key.BaseSHA},
		"head_sha":         {key.HeadSHA},
		"finding_id":       {"auth-1"},
		"status":           {securityaudit.DispositionFalsePositive},
		"evidence":         {"The route requires administrator authorization before the affected call is reachable."},
		"confirm":          {"true"},
		"service_identity": {"implementation-agent"},
	}
	path := "/api/v1/projects/detent/security-audits/dispositions"

	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, path, formEncodedReader(form))
	unauthorizedRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	server.Handler().ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d; body = %s", unauthorized.Code, http.StatusUnauthorized, unauthorized.Body.String())
	}

	global.APIToken = "current-operator-token"
	stale := httptest.NewRecorder()
	staleRequest := httptest.NewRequest(http.MethodPost, path, formEncodedReader(form))
	staleRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	staleRequest.Header.Set("Authorization", "Bearer old-operator-token")
	server.Handler().ServeHTTP(stale, staleRequest)
	if stale.Code != http.StatusUnauthorized {
		t.Fatalf("stale token status = %d, want %d; body = %s", stale.Code, http.StatusUnauthorized, stale.Body.String())
	}

	// The MCP path must enforce exact-head evidence and return a real pending
	// approval, without accepting model-supplied confirmation or writing a disposition.
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: now, BoardIssues: []telemetry.Issue{{ProjectID: "detent", ID: "issue-2005", Identifier: "digitaldrywood/detent#2005", State: "Todo"}}}); err != nil {
		t.Fatal(err)
	}
	mcpHeaders := map[string]string{"Authorization": "Bearer current-operator-token"}
	connection := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, mcpHeaders)
	var setup struct {
		ID string `json:"connection_id"`
	}
	if err := json.Unmarshal(connection.Body.Bytes(), &setup); err != nil || setup.ID == "" {
		t.Fatalf("connection=%s %v", connection.Body, err)
	}
	mcpHeaders["X-Detent-Connection-ID"] = setup.ID
	for _, tt := range []struct {
		name, head string
		success    bool
	}{{"untrusted head", "other", false}, {"exact trusted head", key.HeadSHA, true}} {
		t.Run(tt.name, func(t *testing.T) {
			input := map[string]any{"project_id": "detent", "identifier": "digitaldrywood/detent#2005", "request_id": tt.name, "repository": key.Repository, "pull_request": key.PRNumber, "base_sha": key.BaseSHA, "head_sha": tt.head, "finding_id": "auth-2", "evidence": "Verified false positive"}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/dispose_security_finding", string(raw), mcpHeaders)
			if strings.Contains(response.Body.String(), `"status":"succeeded"`) != tt.success {
				t.Fatalf("disposition=%d %s", response.Code, response.Body)
			}
			if dispositions, err := deps.Store.ListSecurityAuditDispositions(t.Context(), run.ID); err != nil || len(dispositions) != map[bool]int{true: 1, false: 0}[tt.success] {
				t.Fatalf("authority outcome=%+v %v", dispositions, err)
			}
		})
	}
	workerToken := workerCredentials.Token("detent")
	capacity := httptest.NewRecorder()
	capacityRequest := httptest.NewRequest(http.MethodPost, "/api/v1/capacity/clear", nil)
	capacityRequest.Header.Set("Authorization", "Bearer "+workerToken)
	server.Handler().ServeHTTP(capacity, capacityRequest)
	if capacity.Code != http.StatusForbidden {
		t.Fatalf("worker capacity status = %d, want %d; body = %s", capacity.Code, http.StatusForbidden, capacity.Body.String())
	}

	otherProject := httptest.NewRecorder()
	otherProjectRequest := httptest.NewRequest(http.MethodPost, "/api/v1/projects/other/security-audits/dispositions", formEncodedReader(form))
	otherProjectRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	otherProjectRequest.Header.Set("Authorization", "Bearer "+workerToken)
	server.Handler().ServeHTTP(otherProject, otherProjectRequest)
	if otherProject.Code != http.StatusForbidden {
		t.Fatalf("other project status = %d, want %d; body = %s", otherProject.Code, http.StatusForbidden, otherProject.Body.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, formEncodedReader(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer current-operator-token")
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}

	workerForm := form
	workerForm.Set("finding_id", "auth-2")
	worker := httptest.NewRecorder()
	workerRequest := httptest.NewRequest(http.MethodPost, path, formEncodedReader(workerForm))
	workerRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	workerRequest.Header.Set("Authorization", "Bearer "+workerToken)
	server.Handler().ServeHTTP(worker, workerRequest)
	if worker.Code != http.StatusCreated {
		t.Fatalf("worker status = %d, want %d; body = %s", worker.Code, http.StatusCreated, worker.Body.String())
	}
	dispositions, err := deps.Store.ListSecurityAuditDispositions(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("ListSecurityAuditDispositions() error = %v", err)
	}
	if len(dispositions) != 3 || dispositions[0].ServiceIdentity != "detent:detent" || dispositions[1].ServiceIdentity != "detent:detent" {
		t.Fatalf("dispositions = %#v", dispositions)
	}
	if evaluation := securityaudit.Evaluate(run, dispositions, key, "detent:detent", []string{"p1", "p2"}); !evaluation.Allowed {
		t.Fatalf("Evaluate() = %#v, want allowed", evaluation)
	}
}

func formEncodedReader(values url.Values) *strings.Reader {
	return strings.NewReader(values.Encode())
}
