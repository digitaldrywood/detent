package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func TestAuditEvidenceResultOmitsCredentialsAndRawFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	result := auditEvidenceResultFromRun(securityaudit.Run{
		ID:                 7,
		InvocationID:       "invocation-7",
		ProjectID:          "detent",
		Repository:         "digitaldrywood/detent",
		PRNumber:           2006,
		BaseSHA:            "base-7",
		HeadSHA:            "head-7",
		ServiceIdentity:    "detent:detent",
		ReviewerVersion:    securityaudit.ReviewerVersion,
		ReviewerDigest:     securityaudit.ReviewerDigest(),
		AuthenticationMode: securityaudit.AuthenticationSubscription,
		WorkerPID:          987654,
		ProviderThreadID:   "private-provider-thread",
		ExitStatus:         securityaudit.ExitStatusFailed,
		Failure:            "credential=must-not-be-exposed",
		OutputDigest:       securityaudit.OutputDigest("output"),
		OutputBytes:        6,
		Verdict:            securityaudit.VerdictFail,
		Summary:            "One finding.",
		Findings:           []securityaudit.Finding{{ID: "auth-1", Severity: "p1", Body: "Missing authorization."}},
		Attempt:            1,
		StartedAt:          now,
		CompletedAt:        now.Add(time.Second),
		RecordedAt:         now.Add(time.Second),
	}, nil)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	encoded := string(raw)
	for _, forbidden := range []string{"must-not-be-exposed", "private-provider-thread", "987654"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("audit evidence exposes %q: %s", forbidden, encoded)
		}
	}
	for _, required := range []string{"invocation-7", "base-7", "head-7", securityaudit.AuthenticationSubscription} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("audit evidence missing %q: %s", required, encoded)
		}
	}
}

func TestAuditEvidenceRequiresBaseAndHeadTogether(t *testing.T) {
	t.Parallel()

	configPath := ""
	cmd := newAuditEvidenceCommand(&configPath, defaultOptions())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--project", "detent",
		"--repository", "digitaldrywood/detent",
		"--pull-request", "2006",
		"--base-sha", "base-7",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--base-sha and --head-sha") {
		t.Fatalf("Execute() error = %v, want exact SHA pair validation", err)
	}
}

func TestRunAuditDispositionUsesAuthenticatedExactHeadServicePath(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/projects/detent/security-audits/dispositions" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		for key, want := range map[string]string{
			"repository":   "digitaldrywood/detent",
			"pull_request": "2006",
			"base_sha":     "base-7",
			"head_sha":     "head-7",
			"finding_id":   "auth-1",
			"status":       securityaudit.DispositionFalsePositive,
			"evidence":     "The route is unreachable before administrator authorization.",
			"confirm":      "true",
		} {
			if got := request.Form.Get(key); got != want {
				t.Fatalf("form[%q] = %q, want %q", key, got, want)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(securityaudit.Disposition{
			ID:              9,
			AuditRunID:      7,
			FindingID:       "auth-1",
			Status:          securityaudit.DispositionFalsePositive,
			Evidence:        request.Form.Get("evidence"),
			ServiceIdentity: "detent:detent",
			RecordedAt:      now,
		})
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatalf("strconv.Atoi() error = %v", err)
	}

	opts := dashboardAddressOptions(globalconfig.Config{APIToken: "operator-token"})
	opts.httpDo = server.Client().Do
	result, err := runAuditDisposition(t.Context(), "/config/global.yaml", serverURL.Hostname(), port, true, "detent", "digitaldrywood/detent", 2006, "base-7", "head-7", "auth-1", "The route is unreachable before administrator authorization.", opts)
	if err != nil {
		t.Fatalf("runAuditDisposition() error = %v", err)
	}
	if result.ID != 9 || result.AuditRunID != 7 || result.ServiceIdentity != "detent:detent" {
		t.Fatalf("result = %#v", result)
	}
}

func TestAuditDispositionRequiresConfirmation(t *testing.T) {
	t.Parallel()

	configPath := ""
	host := ""
	port := -1
	cmd := newAuditDispositionCommand(&configPath, &host, &port, defaultOptions())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--project", "detent",
		"--repository", "digitaldrywood/detent",
		"--pull-request", "2006",
		"--base-sha", "base-7",
		"--head-sha", "head-7",
		"--finding", "auth-1",
		"--evidence", "Verified administrator-only path.",
	})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires --yes") {
		t.Fatalf("Execute() error = %v, want confirmation validation", err)
	}
}
