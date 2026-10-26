package tracker

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
)

func TestPublicGateResultRedactsCheckEvidence(t *testing.T) {
	githubToken := "ghp_" + strings.Repeat("1234567890", 3)
	timeout := "panic: test timed out after 1m0s\nrunning tests:\n\tTestGateReceipt/世界 (1m0s)\n\n" +
		"goroutine 1 [chan receive]:\ngithub.com/example-org/example-project/internal/worker.TestGateReceipt(0xc000001)\n\t/Users/private/project/internal/worker/worker_test.go:42 +0x12\n" +
		"Authorization: Bearer private-bearer-token\nDEPLOY_TOKEN=private-assignment\nhttps://private.example/log?secret=private-url\n" +
		strings.Repeat("goroutine 99 [select]:\n\t/home/private/project/worker.go:12 +0x1 世界\n", 300) +
		"FAIL\tgithub.com/example-org/example-project/internal/worker\t61.366s\n"
	tests := []struct {
		name          string
		resultCommand string
		command       string
		scope         string
		output        string
		wantCommand   string
		wantOutput    string
		wantChecks    int
		wantContains  []string
		wantTruncated bool
		wasTruncated  bool
	}{
		{name: "separated failures", output: "--- FAIL: TestFirst (0.01s)\n" + strings.Repeat("ok example.org/passing\n", 4000) + "--- FAIL: TestSecond (0.02s)\ninternal/example.go:12:3: unchecked result (errcheck)\n" + strings.Repeat("ok example.org/passing\n", 4000) + "FAIL\texample.org/second\t1s\n", wantChecks: 1, wantTruncated: true, wantContains: []string{"--- FAIL: TestFirst (0.01s)\n", "--- FAIL: TestSecond (0.02s)\n", "internal/example.go:12:3: unchecked result (errcheck)\n", "FAIL\texample.org/second\t1s\n"}},
		{name: "terminal make failure preserves preceding diagnostic", output: strings.Repeat(" PASS frontend suite 世界\n", 1000) + "./scripts/check-repository.sh\nRepository schema mismatch 世界\nmigrations/example.sql: missing delivery column\nexpected column: delivery_id\nactual column: legacy_id\nTOKEN=private-assignment\n/home/private/project/build.log\nmake: *** [check-repository] Error 2\n", wantChecks: 1, wantTruncated: true, wantContains: []string{"./scripts/check-repository.sh", "Repository schema mismatch 世界", "migrations/example.sql: missing delivery column", "expected column: delivery_id", "actual column: legacy_id", "make: *** [check-repository] Error 2", "[redacted]"}},
		{name: "colored frontend failure before passing output", output: "\x1b[31m FAIL \x1b[0m packages/ui/selection.test.tsx > selects a range\nAssertionError: expected range but received null\n" + strings.Repeat(" PASS later suite\n", 1000), wantChecks: 1, wantTruncated: true, wantContains: []string{"selection.test.tsx", "AssertionError: expected range but received null"}},
		{name: "legacy npm build failure", output: "npm ERR! code ELIFECYCLE\nnpm ERR! command failed\n" + strings.Repeat(" PASS frontend suite 世界\n", 8000), wantChecks: 1, wantTruncated: true, wantContains: []string{"npm ERR! code ELIFECYCLE", "npm ERR! command failed"}},
		{name: "pnpm build failure", output: "ERR_PNPM_RECURSIVE_RUN_FIRST_FAIL build: `vite build`\n" + strings.Repeat(" PASS frontend suite 世界\n", 8000), wantChecks: 1, wantTruncated: true, wantContains: []string{"ERR_PNPM_RECURSIVE_RUN_FIRST_FAIL"}},
		{name: "compiler location", output: "src/widget.tsx:12:7 - error TS2322: Type mismatch 世界\n" + strings.Repeat(" PASS frontend suite 世界\n", 8000), wantChecks: 1, wantTruncated: true, wantContains: []string{"src/widget.tsx:12:7 - error TS2322", "世界"}},
		{name: "vite build failure", output: "error during build:\n[vite:esbuild] Transform failed with 1 error:\nsrc/widget.ts:12:7: ERROR: Unexpected token\n" + strings.Repeat(" PASS frontend suite 世界\n", 8000), wantChecks: 1, wantTruncated: true, wantContains: []string{"error during build:", "[vite:esbuild] Transform failed", "src/widget.ts:12:7: ERROR: Unexpected token"}},
		{name: "successful stage does not hide later failure", output: "check-fast-stage stage=repository exit_status=0\n" + strings.Repeat(" PASS frontend suite 世界\n", 1000) + "Error: repository check rejected migration\n" + strings.Repeat(" PASS later suite\n", 1000), wantChecks: 1, wantTruncated: true, wantContains: []string{"Error: repository check rejected migration"}},
		{name: "colored secret assignments", output: "TOK\x1b[31mEN=private-assignment\x1b[0m\nAuthorization: \x1b[31mBearer private-bearer-token\x1b[0m\n", wantChecks: 1, wantOutput: "TOKEN=[redacted]\nAuthorization: [redacted]\n"},
		{name: "assertion without failure header", output: "AssertionError: expected 世界 but received null\n" + strings.Repeat(" PASS later suite\n", 1000), wantChecks: 1, wantTruncated: true, wantContains: []string{"AssertionError: expected 世界 but received null"}},
		{name: "Bearer output", output: "Bearer private-bearer-token", wantCommand: "go test internalexample", wantOutput: "Bearer [redacted]", wantChecks: 1},
		{name: "ghp token in check command", command: "go test " + githubToken, wantOutput: "", wantChecks: 0},
		{name: "ghp token in command", resultCommand: "go test " + githubToken, wantCommand: "go test [redacted]", wantOutput: "", wantChecks: 1},
		{name: "sk token in check scope", scope: "sk-123456789012345678901234567890", wantOutput: "", wantChecks: 0},
		{name: "private scope is dropped", scope: "/Users/private/project", wantOutput: "", wantChecks: 0},
		{name: "sk output", output: "sk-123456789012345678901234567890", wantCommand: "go test internalexample", wantOutput: "[redacted]", wantChecks: 1},
		{name: "source diagnostic identity", output: "--- FAIL: TestGateReceipt/child (0.01s)\n    internal/worker/worker_test.go:42: failed\nFAIL github.com/example-org/example-project/internal/worker 1s\n", wantChecks: 1, wantContains: []string{"TestGateReceipt/child", "internal/worker/worker_test.go:42", "FAIL github.com/example-org/example-project/internal/worker"}},
		{name: "private host paths", output: "path=/Users/private/project/file.go C:\\Users\\private\\file.go \\\\private\\share\\file.go ~/private/file.go (/home/private/file.go) file:///private/file.go", wantChecks: 1, wantContains: []string{"[redacted]"}},
		{name: "long timeout", output: timeout, wantChecks: 1, wantTruncated: true, wantContains: []string{"panic: test timed out after 1m0s", "TestGateReceipt/世界", "github.com/example-org/example-project/internal/worker.TestGateReceipt", "FAIL\tgithub.com/example-org/example-project/internal/worker\t61.366s", "[redacted]", "truncated"}},
		{name: "long timeout after build output", output: strings.Repeat("ok example.org/other/package\n", 300) + timeout, wantChecks: 1, wantTruncated: true, wantContains: []string{"panic: test timed out after 1m0s", "TestGateReceipt/世界", "FAIL\tgithub.com/example-org/example-project/internal/worker\t61.366s", "truncated"}},
		{name: "failure near end", output: strings.Repeat("世界\n", 1000) + "--- FAIL: TestGateReceipt/child (0.01s)\n    worker_test.go:42: connection refused\nFAIL example.org/worker 1s\n", wantChecks: 1, wantTruncated: true, wantContains: []string{"TestGateReceipt/child", "connection refused", "FAIL example.org/worker", "[earlier output truncated]"}},
		{name: "long ordinary failure", output: "--- FAIL: TestGateReceipt/child (0.01s)\n    worker_test.go:42: connection refused\n" + strings.Repeat("diagnostic 世界\n", 1000) + "FAIL example.org/worker 1s\n", wantChecks: 1, wantTruncated: true, wantContains: []string{"TestGateReceipt/child", "connection refused", "FAIL example.org/worker", "truncated"}},
		{name: "unavailable original diagnostic", output: "[earlier output truncated]\n" + strings.Repeat("界", 1356), wantOutput: "[earlier output truncated]\n" + strings.Repeat("界", 1356), wantChecks: 1, wasTruncated: true, wantTruncated: true},
		{name: "invalid UTF-8", output: "failed 世界\xff", wantOutput: "failed 世界�", wantChecks: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resultCommand := valueOr(test.resultCommand, "go test internalexample")
			wantCommand := valueOr(test.wantCommand, resultCommand)
			result := gate.CommandResult{
				Command:         resultCommand,
				HeadSHA:         strings.Repeat("a", 40),
				TreeSHA:         strings.Repeat("b", 40),
				ExitCode:        1,
				Output:          test.output,
				OutputTruncated: test.wasTruncated,
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
			if test.wantContains == nil && got.Output != test.wantOutput {
				t.Fatalf("PublicGateResult output = %q, want %q", got.Output, test.wantOutput)
			}
			for _, want := range test.wantContains {
				if !strings.Contains(got.Output, want) {
					t.Fatalf("PublicGateResult output missing %q", want)
				}
			}
			for _, secret := range []string{"private", "123456789012345678901234567890"} {
				if strings.Contains(got.Output, secret) {
					t.Fatalf("PublicGateResult output leaked %q", secret)
				}
			}
			if got.OutputTruncated != test.wantTruncated || len(got.Output) > NativeFinalizationTextLimit || !utf8.ValidString(got.Output) || got.ExitCode != 1 || got.HeadSHA != result.HeadSHA || got.TreeSHA != result.TreeSHA {
				t.Fatal("PublicGateResult lost output bounds or failed receipt identity")
			}
			again := PublicGateResult(got)
			if again.Output != got.Output || again.OutputTruncated != got.OutputTruncated || result.Output != test.output {
				t.Fatal("public projection changed original output or was not stable")
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
