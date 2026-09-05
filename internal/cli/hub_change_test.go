package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubChangeCommandsUseScopedNativeAPI(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		action string
		args   []string
		method string
		path   string
		input  string
		output string
	}{
		{"changes", []string{"wi_example"}, "GET", "/work-items/wi_example/changes", "", "[]"},
		{"change", []string{"wi_example", "change_example"}, "GET", "/work-items/wi_example/changes/change_example", "", "{}"},
		{"create-change", []string{"wi_example"}, "POST", "/work-items/wi_example/changes", `{"idempotency_key":"command","title":"Native change","body":"Discuss"}`, "{}"},
		{"publish-change", []string{"wi_example", "change_example"}, "POST", "/work-items/wi_example/changes/change_example/versions", `{"idempotency_key":"command","expected_version_id":"version_previous","head_sha":"head"}`, "{}"},
		{"review-change", []string{"wi_example", "change_example", "version_example"}, "POST", "/work-items/wi_example/changes/change_example/versions/version_example/reviews", `{"idempotency_key":"command","decision":"approved"}`, "{}"},
		{"check-change", []string{"wi_example", "change_example", "version_example"}, "POST", "/work-items/wi_example/changes/change_example/versions/version_example/checks", `{"idempotency_key":"command","check_run_id":"check_example","source":"independent"}`, "{}"},
		{"discuss-change", []string{"wi_example", "change_example"}, "POST", "/work-items/wi_example/changes/change_example/discussion", `{"idempotency_key":"command","version_id":"version_example","body":"Comment"}`, "{}"},
		{"approve-review-policy", nil, "PUT", "/change-review-policy", `{"idempotency_key":"command","expected_review_policy_id":"previous","policy":{"policy_id":"policy_example","require_review":true}}`, "{}"},
	} {
		t.Run(test.action, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != test.method || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example"+test.path || r.Header.Get("Authorization") != "Bearer scoped-token" {
					t.Errorf("unexpected scoped request: %s %s", r.Method, r.URL)
				}
				if test.input != "" {
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || string(body["idempotency_key"]) != `"command"` {
						t.Errorf("request lost mutation identity: %v", err)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if _, err := io.WriteString(w, test.output); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			cmd := newHubIssueCommand(func(string) string { return "scoped-token" })
			args := append([]string{test.action, "--hub-url", server.URL, "--organization", "org_example", "--project", "prj_example"}, test.args...)
			cmd.SetArgs(args)
			cmd.SetIn(strings.NewReader(test.input))
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("native calls = %d, want 1", calls)
			}
		})
	}
}
