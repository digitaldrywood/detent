package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFleetPaginationRejectsIncompleteResults(t *testing.T) {
	for _, tc := range []struct {
		name, next string
		denied     bool
	}{
		{"complete", "1", false}, {"malformed", "invalid", false}, {"repeated", "0", false}, {"gap", "2", false}, {"out of range", "100001", false}, {"second page denied", "1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client, err := New(Config{URL: "http://hub.test", TokenSource: func() string { return "example" }, HTTPClient: providerHandlerClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/policy") {
					if err := json.NewEncoder(w).Encode(policy.Approval{Policy: policy.Descriptor{ID: "approved"}}); err != nil {
						t.Error(err)
					}
					return
				}
				calls++
				if calls > 2 || calls == 2 && tc.denied {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if calls == 1 {
					w.Header().Set("X-Next-Offset", tc.next)
				}
				if err := json.NewEncoder(w).Encode([]runnerauth.Runner{{Binding: runnerauth.Binding{RunnerID: "runner_" + strconv.Itoa(calls)}}}); err != nil {
					t.Error(err)
				}
			}))})
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := NewFleetClient(client, "org_example", map[string]tracker.ProjectID{"example": "prj_example"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.ProjectEligibility(t.Context(), "example")
			if tc.name == "complete" {
				if err != nil || len(result.Runners) != 2 || calls != 2 {
					t.Fatalf("eligibility=%+v calls=%d error=%v", result, calls, err)
				}
			} else if err == nil || len(result.Runners) != 0 || calls > 2 {
				t.Fatalf("incomplete eligibility accepted: %+v calls=%d error=%v", result, calls, err)
			}
		})
	}
}
