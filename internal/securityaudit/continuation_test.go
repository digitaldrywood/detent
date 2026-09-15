package securityaudit

import (
	"strings"
	"testing"
)

func TestContinuation(t *testing.T) {
	t.Parallel()
	previous := &PreviousAudit{BaseSHA: "base", HeadSHA: "old", Verdict: VerdictFail, Findings: []Finding{{ID: "auth", Severity: "p1", Body: "authorization missing", Path: "auth.go"}}}
	for _, tt := range []struct {
		name, output string
		wantErr      bool
	}{
		{"resolved", `{"verdict":"pass","summary":"fixed","findings":[{"id":"auth","severity":"p1","body":"authorization added","status":"resolved"}]}`, false},
		{"unresolved", `{"verdict":"fail","summary":"still present","findings":[{"id":"auth","severity":"p1","body":"authorization missing","status":"unresolved"}]}`, false},
		{"omitted", `{"verdict":"pass","summary":"fixed","findings":[]}`, true},
		{"no status", `{"verdict":"fail","summary":"still present","findings":[{"id":"auth","severity":"p1","body":"authorization missing"}]}`, true},
		{"downgraded", `{"verdict":"fail","summary":"still present","findings":[{"id":"auth","severity":"p3","body":"authorization missing","status":"unresolved"}]}`, true},
		{"invalid status", `{"verdict":"pass","summary":"fixed","findings":[{"id":"auth","severity":"p1","body":"authorization added","status":"ignored"}]}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseOutput(tt.output)
			if err == nil {
				err = ValidateContinuation(previous, result)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, tt := range []struct {
		name    string
		limit   int
		wantErr bool
	}{{"bounded", 4096, false}, {"oversized", 10, true}} {
		t.Run(tt.name, func(t *testing.T) {
			prompt, err := BuildPrompt(Snapshot{Repository: "owner/repo", PRNumber: 1, BaseSHA: "base", HeadSHA: "new", Previous: previous, FindingFiles: map[string]string{"auth.go": "checked"}}, tt.limit)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err == nil && (!strings.Contains(prompt, `"previous_audit"`) || !strings.Contains(prompt, `"finding_files"`)) {
				t.Fatal("missing continuation context")
			}
		})
	}
}

func TestFullAuditCannotResolveFindings(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"", "unresolved", "resolved"} {
		t.Run(status, func(t *testing.T) {
			err := ValidateContinuation(nil, Result{Findings: []Finding{{Status: status}}})
			if (err != nil) != (status == "resolved") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
