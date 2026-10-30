package progresscheck

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type ciBackend func(context.Context, string, string) (string, error)

func (b ciBackend) Generate(ctx context.Context, instructions, input string) (string, error) {
	return b(ctx, instructions, input)
}

func TestCIClassificationValidatesEvidenceAndAnswer(t *testing.T) {
	for _, test := range []struct {
		name, output string
		oversized    bool
		wantErr      bool
	}{
		{"source", `{"outcome":"source","target":"TestExample","reason":"assertion failed"}`, false, false},
		{"infrastructure", `{"outcome":"infrastructure","target":"runner","reason":"runner connection lost"}`, false, false},
		{"unknown", `{"outcome":"unknown","target":"","reason":"log unavailable"}`, false, false},
		{"missing target", `{"outcome":"source","reason":"assertion failed"}`, false, true},
		{"missing external cause", `{"outcome":"infrastructure","reason":"external failure"}`, false, true},
		{"missing explanation", `{"outcome":"unknown"}`, false, true},
		{"invalid outcome", `{"outcome":"guess","target":"TestExample","reason":"maybe"}`, false, true},
		{"extra field", `{"outcome":"unknown","reason":"no logs","state":"Blocked"}`, false, true},
		{"trailing data", `{"outcome":"unknown","reason":"no logs"} {}`, false, true},
		{"oversized output", strings.Repeat("x", MaxOutputBytes+1), false, true},
		{"oversized input", "", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := CIFailure{Repository: "owner/repository", HeadSHA: "head", RunID: "1", RunAttempt: "1", JobID: "2", JobName: "check", ExitStatus: "failure", LogTail: "untrusted evidence"}
			if test.oversized {
				request.LogTail = strings.Repeat("x", 8001)
			}
			calls := 0
			backend := ciBackend(func(_ context.Context, instructions, input string) (string, error) {
				calls++
				var evidence CIFailure
				if instructions != CIInstructions || json.Unmarshal([]byte(input), &evidence) != nil || evidence.LogTail != request.LogTail {
					t.Fatal("AI did not receive bounded evidence as data")
				}
				return test.output, nil
			})
			_, err := (AI{Backend: backend}).ClassifyCI(t.Context(), request)
			if (err != nil) != test.wantErr || test.oversized && calls != 0 {
				t.Fatalf("classification error=%v calls=%d", err, calls)
			}
		})
	}
}
