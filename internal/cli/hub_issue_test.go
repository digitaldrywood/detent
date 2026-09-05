package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubIssueRoutesMutationsToNativeAPI(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ action, method, suffix, input string }{
		{"create", "POST", "/work-items", `{"idempotency_key":"create","title":"Native","body":"Body","state":"Todo"}`},
		{"edit", "PATCH", "/work-items/wi_example", `{"idempotency_key":"edit","expected_revision":"2","body":"Native edit"}`},
		{"comment", "POST", "/work-items/wi_example/comments", `{"idempotency_key":"comment","body":"Native discussion"}`},
		{"transition", "POST", "/work-items/wi_example/workflow", `{"idempotency_key":"transition","expected_revision":"2","state":"Done","reason":"Finished"}`},
		{"dependency", "POST", "/work-items/wi_example/dependencies", `{"idempotency_key":"dep","expected_revision":"2","related_work_item_id":"wi_blocker","operation":"add"}`},
	} {
		t.Run(test.action, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != test.method || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example"+test.suffix {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer scoped-token" {
					t.Error("missing scoped Hub credential")
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, `{}`); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			cmd := newHubIssueCommand(func(string) string { return "scoped-token" })
			args := []string{test.action, "--hub-url", server.URL, "--organization", "org_example", "--project", "prj_example"}
			if test.action != "create" {
				args = append(args, "wi_example")
			}
			cmd.SetArgs(args)
			cmd.SetIn(strings.NewReader(test.input))
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("API calls = %d, want one native call", calls)
			}
		})
	}
}

func TestNativeIssueInputRejectsInvalidRequests(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`{"unknown":true}`, `{} {}`, strings.Repeat("x", (1<<20)+1)} {
		t.Run(input[:min(len(input), 20)], func(t *testing.T) {
			_, err := nativeIssueInput(strings.NewReader(input), func(struct {
				Required string `json:"required"`
			}) (any, error) {
				t.Fatal("invalid input reached native API")
				return nil, io.ErrUnexpectedEOF
			})
			if err == nil {
				t.Fatal("invalid input succeeded")
			}
		})
	}
}
