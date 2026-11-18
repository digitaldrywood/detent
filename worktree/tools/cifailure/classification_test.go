package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/progresscheck"
)

func TestRecordedDecisionControlsFailureRouting(t *testing.T) {
	for _, test := range []struct {
		name, log, outcome, target string
	}{
		{"Go test", "--- FAIL: TestExample (0s)\nFAIL\towner/repository/pkg\t0s", "source", "TestExample"},
		{"golangci-lint", "level=error msg=unused variable", "source", "golangci-lint"},
		{"Vitest", "FAIL src/example.test.ts > saves changes", "source", "src/example.test.ts"},
		{"network with source location", "internal/client.go:42:3: download failed: network timeout", "infrastructure", "dependency download timeout"},
		{"runner lost", "The runner has lost communication with the server", "infrastructure", "runner connection lost"},
		{"unknown", "Process exited without diagnostics", "unknown", ""},
		{"AI overrides apparent Go failure", "--- FAIL: TestExample (0s)\nFAIL\towner/repository/pkg\t0s", "infrastructure", "runner disk unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeCloud{}
			calls := 0
			command := func(ctx context.Context, name string, args map[string]any, result any) error {
				if name != "classify_ci_failure" {
					return f.command(ctx, name, args, result)
				}
				calls++
				raw, err := json.Marshal(map[string]any{"outcome": test.outcome, "target": test.target, "reason": "Recorded AI explanation"})
				if err != nil {
					return err
				}
				return json.Unmarshal(raw, result)
			}
			gh := &fakeGH{logs: map[int64]string{1: test.log}}
			input := `[{"id":1,"name":"Example check","conclusion":"failure","steps":[{"name":"Run check","conclusion":"failure"}]}]`
			for range 2 {
				destination := &cloudDestination{command: command, project: scheduledCloudProject}
				if err := reportTo(t.Context(), strings.NewReader(input), gh.command, scheduledEnv, destination); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.items) != 1 || len(f.writes) != 1 || calls == 0 {
				t.Fatalf("items=%d writes=%d classification requests=%d", len(f.items), len(f.writes), calls)
			}
			item := f.items[0]
			if slices.Contains(item.Labels, "ci-infrastructure-failure") != (test.outcome == "infrastructure") {
				t.Fatalf("decision %s routed to labels %v", test.outcome, item.Labels)
			}
			if !strings.Contains(item.Body, "Recorded AI explanation") || !strings.Contains(item.Body, fmt.Sprintf(`"outcome":"%s"`, test.outcome)) {
				t.Fatal("recorded classification missing from issue evidence")
			}
			if strings.Contains(item.Body, "No source repair is authorized") != (test.outcome == "infrastructure") {
				t.Fatal("repair authorization disagrees with recorded classification")
			}
		})
	}
}

func TestCIClassificationBoundsFailureEvidence(t *testing.T) {
	for _, test := range []struct {
		name, text, status string
		wantErr            bool
	}{
		{"verbose", strings.Repeat("old output\n", 10000), "failure", false},
		{"escaped", strings.Repeat("\x00", 10000), "failure", false},
		{"unicode", strings.Repeat("界", 10000), "failure", false},
		{"oversized identity", "", strings.Repeat("x", progresscheck.MaxInputBytes), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := make([]string, 200)
			for i := range files {
				files[i] = strings.Repeat("\x00", 1024)
			}
			seen := false
			destination := &cloudDestination{project: scheduledCloudProject, command: func(_ context.Context, name string, args map[string]any, result any) error {
				if name != "classify_ci_failure" {
					t.Fatalf("unexpected operation %s", name)
				}
				seen = true
				request := args["failure"].(progresscheck.CIFailure)
				encoded, err := json.Marshal(request)
				if err != nil || len(encoded) > progresscheck.MaxInputBytes || request.Validate() != nil || !utf8.ValidString(request.LogTail) || !strings.HasSuffix(request.LogTail, "Process completed with exit code 1.") || request.ExitCode == nil || *request.ExitCode != 1 || len(request.ChangedFiles) == 0 || len(request.ChangedFiles) > 100 {
					t.Fatalf("invalid bounded evidence: bytes=%d files=%d", len(encoded), len(request.ChangedFiles))
				}
				return json.Unmarshal([]byte(`{"outcome":"unknown","target":"","reason":"insufficient evidence"}`), result)
			}}
			_, err := classifyFailure(t.Context(), destination, job{ID: 1, Name: "check", Conclusion: test.status}, []byte(test.text+"Process completed with exit code 1."), nil, files, "", scheduledEnv)
			if (err != nil) != test.wantErr || seen == test.wantErr {
				t.Fatalf("classification = %v, requested=%v", err, seen)
			}
		})
	}
}
