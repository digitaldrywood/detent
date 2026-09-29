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
	const repo = "fixture/mobile"
	snapshot := candidatePRFixtureSnapshot(repo, 1)
	snapshot["mergeStateStatus"] = "UNSTABLE"
	check := candidateFixtureCheck(snapshot)
	check["status"], check["conclusion"] = "IN_PROGRESS", ""
	completed := false
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
				status, conclusion := "in_progress", ""
				if completed {
					status, conclusion = "completed", "success"
				}
				write(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "unit", "status": status, "conclusion": conclusion, "started_at": "2026-09-25T12:00:00Z"}}})
			case strings.HasSuffix(r.URL.Path, "/statuses"):
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
	completed = true
	snapshot["mergeStateStatus"] = "CLEAN"
	check["status"], check["conclusion"] = "COMPLETED", "SUCCESS"
	second := refresh()
	if second.HeadSHA != first.HeadSHA || second.BaseSHA != first.BaseSHA || second.MergeableState != "CLEAN" || !strings.EqualFold(second.CI.State, "success") || statusReads != 2 {
		t.Fatalf("completed PR = %+v; reads = %d", second, statusReads)
	}
	decision := gate.Evaluate(gate.Config{Kind: gate.KindCommand, SecurityAudit: gate.SecurityAuditConfig{Enabled: true}}, nil, gate.Summary{PullRequestPresent: true, CIStatus: "pass"}, time.Now(), gate.EvaluationOptions{})
	if decision.Reason != gate.ReasonSecurityAuditMissing {
		t.Fatalf("gate decision = %+v, want trusted audit stage", decision)
	}
}
