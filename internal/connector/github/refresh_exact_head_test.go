package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestRetainedRefreshObservesCompletedCheckOnSameHead(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	const repo = "fixture/mobile"
	snapshot := candidatePRFixtureSnapshot(repo, 1)
	snapshot["mergeStateStatus"] = "UNSTABLE"
	check := candidateFixtureCheck(snapshot)
	check["status"], check["conclusion"] = "IN_PROGRESS", ""
	restStatus, restConclusion := "in_progress", ""
	statusReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		write := func(v any) {
			if err := json.NewEncoder(w).Encode(v); err != nil {
				t.Error(err)
			}
		}
		if r.Method != http.MethodPost {
			switch {
			case strings.HasSuffix(r.URL.Path, "/check-runs"):
				write(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "unit", "status": restStatus, "conclusion": restConclusion, "started_at": "2026-09-25T12:00:00Z"}}})
			case strings.HasSuffix(r.URL.Path, "/statuses"):
				write([]any{})
			case strings.HasSuffix(r.URL.Path, "/annotations"):
				write([]any{})
			default:
				t.Errorf("unexpected REST %s", r.URL)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		var req struct{ Query string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		switch {
		case strings.Contains(req.Query, "CandidatePullRequestReferences"):
			write(map[string]any{"data": map[string]any{"nodes": []any{map[string]any{"id": "I1", "closedByPullRequestsReferences": map[string]any{"totalCount": 1, "nodes": []any{candidatePRFixtureReference(repo, 1)}}}}, "repo0": map[string]any{"pullRequests": map[string]any{"nodes": []any{}}}}})
		case strings.Contains(req.Query, "CandidatePullRequestStatus"):
			statusReads++
			write(map[string]any{"data": map[string]any{"pr0": map[string]any{"pullRequest": snapshot}}})
		default:
			t.Errorf("unexpected GraphQL query: %s", req.Query)
			write(map[string]any{"data": map[string]any{}})
		}
	}))
	defer server.Close()
	c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: repo})
	progress := projectItemsScanProgress{
		scan:     connector.IssueStateScan{Issues: []connector.Issue{{ID: "I1", Identifier: repo + "#1", State: "Merging"}}},
		fields:   map[string]projectItemFields{"I1": {itemID: "P1", fields: map[string]string{}}},
		evidence: map[string]githubIssueNode{"I1": {ID: "I1", CandidatePR: &candidatePullRequestEvidence{complete: true, pullRequest: &pullRequestNode{HeadSHA: "head1", BaseSHA: "base", MergeableState: "UNSTABLE", CI: pullRequestCI{State: "PENDING"}}}}},
		hydrated: map[string]bool{"I1": true},
	}
	refresh := func() pullRequestNode {
		t.Helper()
		if err := c.hydrateRefreshPage(t.Context(), &progress, map[string]struct{}{"merging": {}}, func(connector.Issue) bool { return true }, true); err != nil {
			t.Fatal(err)
		}
		pr := progress.evidence["I1"].CandidatePR.pullRequest
		if pr == nil {
			t.Fatal("missing PR")
		}
		return *pr
	}
	first := refresh()
	if first.HeadSHA != "head1" || first.BaseSHA != "base" || first.MergeableState != "UNSTABLE" || !strings.EqualFold(first.CI.State, "pending") {
		t.Fatalf("running PR = %+v", first)
	}
	for i, tt := range []struct {
		name, status, conclusion, mergeability, ciState string
		reason                                          gate.Reason
	}{
		{"still running", "IN_PROGRESS", "", "UNSTABLE", "pending", gate.ReasonCINotGreen},
		{"completed successfully", "COMPLETED", "SUCCESS", "CLEAN", "success", gate.ReasonSecurityAuditMissing},
		{"completed with failure", "COMPLETED", "FAILURE", "UNSTABLE", "failure", gate.ReasonCINotGreen},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot["mergeStateStatus"] = tt.mergeability
			check["status"], check["conclusion"] = tt.status, tt.conclusion
			restStatus, restConclusion = strings.ToLower(tt.status), strings.ToLower(tt.conclusion)
			observed := refresh()
			if observed.HeadSHA != first.HeadSHA || observed.BaseSHA != first.BaseSHA || observed.MergeableState != tt.mergeability || !strings.EqualFold(observed.CI.State, tt.ciState) || statusReads != i+2 {
				t.Fatalf("refreshed PR = %+v; reads = %d", observed, statusReads)
			}
			decision := gate.Evaluate(gate.Config{Kind: gate.KindCommand, SecurityAudit: gate.SecurityAuditConfig{Enabled: true}}, nil, gate.Summary{PullRequestPresent: true, CIStatus: observed.CI.State}, time.Now(), gate.EvaluationOptions{})
			if decision.Reason != tt.reason {
				t.Fatalf("gate decision = %+v, want %s", decision, tt.reason)
			}
		})
	}
}
