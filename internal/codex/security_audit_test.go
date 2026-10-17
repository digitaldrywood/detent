package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func TestSecurityAuditInlineDelivery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		diff        string
		encodedOnly bool
	}{
		{name: "JSON encoding alone exceeds inline budget", diff: strings.Repeat("+\"x\"\n", 59000), encodedOnly: true},
		{name: "raw prompt exceeds inline budget", diff: strings.Repeat("+source line\n", 50000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := securityaudit.Snapshot{ProjectID: "project", IssueID: "issue", Repository: "owner/repository", BaseSHA: "base", HeadSHA: "head", Diff: "diff --git a/file.go b/file.go\n@@ -0,0 +1 @@\n" + test.diff}
			prompt, err := securityaudit.BuildPrompt(snapshot, 2*1024*1024)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(prompt)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("source_bytes=%d prompt_bytes=%d encoded_bytes=%d inline_budget=%d", len(snapshot.Diff), len(prompt), len(encoded), maxTurnInputBytes/2)
			if (test.encodedOnly && len(prompt) > maxTurnInputBytes/2) || len(encoded) <= maxTurnInputBytes/2 {
				t.Fatal("fixture must exceed the intended inline budget")
			}
			plan, err := securityaudit.BuildPlan(snapshot, 2*1024*1024)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Len() <= 1 {
				t.Fatal("configured source limit overrode the inline delivery budget")
			}
			var reconstructed strings.Builder
			for index := range plan.Len() {
				_, prompt := plan.Chunk(index)
				transport := newFakeAppServerTransport([]Message{
					responseMessage(t, 1, `{"userAgent":"codex-cli/0.159.2"}`),
					responseMessage(t, 2, `{"thread":{"id":"audit-thread","model":"audit-model"}}`),
					responseMessage(t, 3, `{"turn":{"id":"audit-turn"}}`),
					notificationMessage(t, "turn/completed", `{"threadId":"audit-thread","turn":{"id":"audit-turn","status":"completed"}}`),
				})
				server, err := NewAppServer(staticTransportFactory{transport: transport}, WithReadTimeout(time.Second), WithTurnTimeout(time.Second))
				if err != nil {
					t.Fatal(err)
				}
				_, err = server.RunTurn(t.Context(), RunTurnRequest{Workspace: t.TempDir(), TempDir: t.TempDir(), Prompt: prompt, DeveloperInstructions: securityaudit.ToolInstructions(), Model: "audit-model"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				turns := 0
				for _, message := range transport.sentMessages() {
					if message.Method != "turn/start" {
						continue
					}
					turns++
					if len(message.Params) >= maxTurnInputBytes {
						t.Fatal("audit exceeded the final turn/start frame budget")
					}
					var params struct {
						Input []struct{ Text string } `json:"input"`
					}
					if err := json.Unmarshal(message.Params, &params); err != nil {
						t.Fatal(err)
					}
					if len(params.Input) != 1 || params.Input[0].Text != prompt {
						t.Fatal("no-tools audit received a file-read request instead of inline source and output schema")
					}
				}
				if turns != 1 {
					t.Fatal("audit turn was not sent")
				}
				_, payload, ok := strings.Cut(prompt, "\n\nAudit payload:\n")
				var input struct {
					Diff string `json:"textual_diff"`
				}
				if !ok || json.Unmarshal([]byte(payload), &input) != nil {
					t.Fatal("missing inline audit payload")
				}
				reconstructed.WriteString(input.Diff)
			}
			if reconstructed.String() != snapshot.Diff {
				t.Fatal("adapter omitted or duplicated audit source bytes")
			}
		})
	}
}
