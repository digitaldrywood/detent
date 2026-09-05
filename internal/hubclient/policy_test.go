package hubclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func clientTestPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

func TestSchedulerRejectsUnapprovedPolicyBeforeWork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		descriptor policy.Descriptor
	}{
		{"missing", policy.Descriptor{}},
		{"changed", func() policy.Descriptor { d := clientTestPolicy(); d.Gates.AutoPromote = true; return d.WithID() }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/policy") {
					claims++
				}
				if err := json.NewEncoder(w).Encode(policy.Approval{Policy: clientTestPolicy()}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "worker" }})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine_a", Hostname: "host", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			_, err = scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "p", Repository: "acme/repo", Policy: test.descriptor})
			if err == nil || !strings.Contains(err.Error(), "policy_mismatch") || claims != 0 {
				t.Fatalf("claims=%d, error=%v", claims, err)
			}
			scheduler.claims["issue"] = tracker.Lease{LeaseSummary: tracker.LeaseSummary{PolicyID: clientTestPolicy().ID}}
			scheduler.claimPolicies["issue"] = claimPolicy{project: "p", repository: "acme/repo", descriptor: test.descriptor}
			if _, err := scheduler.AdoptClaim(t.Context(), connector.Issue{ID: "issue"}, time.Now()); err == nil {
				t.Fatal("adopted mismatched policy")
			}
		})
	}
}
