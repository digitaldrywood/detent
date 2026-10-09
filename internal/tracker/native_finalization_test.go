package tracker

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
)

func TestPublicGateResultRedactsCheckEvidence(t *testing.T) {
	tests := []struct {
		name          string
		resultCommand string
		command       string
		scope         string
		output        string
		wantCommand   string
		wantOutput    string
		wantChecks    int
	}{
		{name: "Bearer output", output: "Bearer private-bearer-token", wantCommand: "go test internalexample", wantOutput: "Bearer [redacted]", wantChecks: 1},
		{name: "ghp token in check command", command: "go test ghp_123456789012345678901234567890", wantOutput: "", wantChecks: 0},
		{name: "ghp token in command", resultCommand: "go test ghp_123456789012345678901234567890", wantCommand: "go test [redacted]", wantOutput: "", wantChecks: 1},
		{name: "sk token in check scope", scope: "sk-123456789012345678901234567890", wantOutput: "", wantChecks: 0},
		{name: "private scope is dropped", scope: "/Users/private/project", wantOutput: "", wantChecks: 0},
		{name: "sk output", output: "sk-123456789012345678901234567890", wantCommand: "go test internalexample", wantOutput: "[redacted]", wantChecks: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resultCommand := valueOr(test.resultCommand, "go test internalexample")
			wantCommand := valueOr(test.wantCommand, resultCommand)
			result := gate.CommandResult{
				Command:  resultCommand,
				HeadSHA:  strings.Repeat("a", 40),
				TreeSHA:  strings.Repeat("b", 40),
				ExitCode: 1,
				Output:   test.output,
				Evidence: &gate.CommandEvidence{Checks: []gate.CheckObservation{{
					Scope:      valueOr(test.scope, "internalexample"),
					Command:    valueOr(test.command, "go test internalexample"),
					HeadSHA:    strings.Repeat("a", 40),
					TreeSHA:    strings.Repeat("b", 40),
					StartedAt:  time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
					FinishedAt: time.Date(2026, 10, 9, 12, 0, 1, 0, time.UTC),
				}}},
			}
			got := PublicGateResult(result)
			if got.Command != wantCommand {
				t.Fatalf("PublicGateResult command = %q, want %q", got.Command, wantCommand)
			}
			if got.Output != test.wantOutput || strings.Contains(got.Output, "private-bearer-token") || strings.Contains(got.Output, "123456789012345678901234567890") {
				t.Fatalf("PublicGateResult output = %q, want %q", got.Output, test.wantOutput)
			}
			if got.Evidence == nil || len(got.Evidence.Checks) != test.wantChecks {
				t.Fatalf("PublicGateResult checks = %+v, want %d", got.Evidence, test.wantChecks)
			}
			if test.wantChecks > 0 {
				check := got.Evidence.Checks[0]
				if strings.Contains(check.Command, "123456789012345678901234567890") || strings.Contains(check.Scope, "123456789012345678901234567890") {
					t.Fatalf("PublicGateResult leaked check evidence: %+v", check)
				}
			}
		})
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
