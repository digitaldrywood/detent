package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
