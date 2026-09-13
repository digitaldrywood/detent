package web_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/serviceapi"
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
	if len(dispositions) != 2 || dispositions[0].ServiceIdentity != "detent:detent" || dispositions[1].ServiceIdentity != "detent:detent" {
		t.Fatalf("dispositions = %#v", dispositions)
	}
	if evaluation := securityaudit.Evaluate(run, dispositions, key, "detent:detent", []string{"p1", "p2"}); !evaluation.Allowed {
		t.Fatalf("Evaluate() = %#v, want allowed", evaluation)
	}
}

func formEncodedReader(values url.Values) *strings.Reader {
	return strings.NewReader(values.Encode())
}
