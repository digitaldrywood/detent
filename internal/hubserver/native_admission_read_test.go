package hubserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeAdmissionExplanation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		label       string
		context     bool
		policy      string
		provider    string
		stale       bool
		oldVersion  bool
		outcome     string
		code        string
		unavailable string
		authority   string
		status      int
	}{
		{name: "known native label refusal", label: "human-owned", context: true, outcome: "skipped", code: "no_claimable_work"},
		{name: "admissible native item", label: "ordinary", context: true, outcome: "ready"},
		{name: "label alone proves no refusal", label: "human-owned", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "approved runner selector refusal", policy: "wrong-runner", outcome: "skipped", code: "selector_no_match"},
		{name: "stale runner policy", context: true, policy: "stale", outcome: "skipped", code: "policy_mismatch"},
		{name: "unknown provider allows concurrency but needs model context", context: true, provider: "unknown", outcome: "unknown", unavailable: "provider_candidate_requirement"},
		{name: "stale provider exhaustion is unknown", context: true, provider: "stale", outcome: "unknown", unavailable: "provider_candidate_requirement"},
		{name: "exhausted provider refuses", context: true, provider: "exhausted", outcome: "skipped", code: "provider_capacity"},
		{name: "stale heartbeat refuses", context: true, stale: true, outcome: "skipped", code: "runner_offline"},
		{name: "old runner version refuses", context: true, oldVersion: true, outcome: "skipped", code: "unavailable"},
		{name: "removed runner project grant", context: true, authority: "grant", status: http.StatusNotFound},
		{name: "revoked runner credential", context: true, authority: "revoked", status: http.StatusUnauthorized},
		{name: "operator cannot assert runner selectors", context: true, authority: "operator", status: http.StatusUnprocessableEntity},
		{name: "bounded selector context", context: true, authority: "oversized", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			issue := f.create(t, "native candidate")
			labelValues := []string{}
			if test.label != "" {
				labelValues = append(labelValues, test.label)
			}
			labels, err := json.Marshal(labelValues)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json=?, body=? WHERE native_id=?", string(labels), "private-body /private/runner credential-sentinel", issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			descriptor := hubTestPolicy()
			if test.policy == "wrong-runner" {
				descriptor.Requirements.RunnerID = runnerauth.NewBinding().RunnerID
				descriptor = descriptor.WithID()
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			if test.provider != "" {
				report := capacityReport(now)
				report.Availability = test.provider
				if test.provider == "stale" {
					report.Availability = "exhausted"
					report.ObservedAt = now.Add(-providercapacity.MaxAge)
				}
				publishCapacity(t, f, r, report)
			}
			if test.stale {
				now = now.Add(runnerauth.HeartbeatTimeout)
			}
			if test.oldVersion {
				f.service.config.Version = "v9.0.0"
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET version='v1.0.0' WHERE id=?", r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			var historyBefore int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events").Scan(&historyBefore); err != nil {
				t.Fatal(err)
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/runtime"
			current := tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: descriptor.ID, WorkflowStates: []string{"Todo"}, LabelExclude: []string{"human-owned"}}
			if test.policy == "stale" {
				current.PolicyID = "policy_stale"
			}
			if test.authority == "oversized" {
				current.LabelExclude = []string{strings.Repeat("x", 8192)}
			}
			if test.context {
				raw, err := json.Marshal(current)
				if err != nil {
					t.Fatal(err)
				}
				path += "?admission=" + url.QueryEscape(string(raw))
			}
			token := r.redemption.Credential
			switch test.authority {
			case "grant":
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id = (SELECT token_id FROM runner_identities WHERE id=?) AND project_id=?", r.binding.RunnerID, f.project.ID); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE id = (SELECT token_id FROM runner_identities WHERE id=?)", formatHubTime(now), r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			case "operator":
				token = f.token
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, token, nil)
			if test.status != 0 {
				requireNativeStatus(t, response, test.status)
				for _, secret := range []string{string(issue.WorkItemID), r.binding.RunnerID, "credential-sentinel", "private-body", r.redemption.Credential} {
					if strings.Contains(response.Body.String(), secret) {
						t.Fatalf("denied admission leaked evidence: %s", response.Body)
					}
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, response, &evidence)
			if len(evidence.Admission) != 1 {
				t.Fatalf("admission is unavailable: %#v", evidence)
			}
			a := evidence.Admission[0]
			if a.RunnerID != r.binding.RunnerID || a.RunnerRevision < 1 || a.PolicyID != descriptor.ID || a.Outcome != test.outcome || a.ReasonCode != test.code || !a.ObservedAt.Equal(now) || a.Source != "native_claim_candidate_snapshot" {
				t.Fatalf("admission=%#v", a)
			}
			if test.unavailable != "" && !slices.Contains(a.Unavailable, test.unavailable) {
				t.Fatalf("missing unavailable predicate: %#v", a)
			}
			if test.name == "known native label refusal" || test.name == "admissible native item" {
				if a.SelectorObservedAt == nil || !a.SelectorObservedAt.Equal(now) || a.SelectorSource != "registered_runner_published_context" {
					t.Fatalf("selector observation lost its source or freshness: %#v", a)
				}
			}
			explanation := explain.FromNativeEvidence(evidence)
			if explanation.Eligibility.Latest != nil || evidence.LatestDecision != nil || !slices.Contains(evidence.Unavailable, "historical_scheduler_decision") || explanation.Eligibility.Source != explain.SourceAvailable {
				t.Fatalf("snapshot became history: %#v", explanation.Eligibility)
			}
			if test.outcome == "skipped" {
				if explanation.Eligibility.State != explain.EligibilityRefused || len(explanation.Eligibility.Refusals) != 1 || explanation.Eligibility.Refusals[0].Historical || explanation.Eligibility.Refusals[0].ReasonCode != test.code || explanation.Eligibility.Refusals[0].RunnerID != r.binding.RunnerID {
					t.Fatalf("refusal missing from explanation: %#v", explanation.Eligibility)
				}
			} else if len(explanation.Eligibility.Refusals) != 0 || evidence.Scheduling.Outcome != test.outcome {
				t.Fatalf("admissible or unknown candidate became refused: %#v", explanation.Eligibility)
			}
			if !test.context {
				ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
				call := hostedContextProtocol(t, f.service, ctx, "http")
				data := hostedContextData(t, call("tools/call", operatortool.ExplainItem, map[string]any{"project_id": string(f.project.ID), "reference": strconv.Itoa(issue.Number)}), false)
				var projected explain.IssueExplanation
				if err := json.Unmarshal(data, &projected); err != nil {
					t.Fatal(err)
				}
				if projected.Eligibility.State != explanation.Eligibility.State || len(projected.Eligibility.Refusals) != len(explanation.Eligibility.Refusals) {
					t.Fatalf("MCP admission differs: %s", data)
				}
			}
			for _, secret := range []string{"private-body", "/private/runner", "credential-sentinel", r.redemption.Credential, f.token} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("private evidence escaped: %s", secret)
				}
			}
			var historyAfter, leases int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events").Scan(&historyAfter); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil || historyAfter != historyBefore || leases != 0 {
				t.Fatalf("explanation wrote scheduling state: history=%d/%d leases=%d error=%v", historyBefore, historyAfter, leases, err)
			}
			if test.name == "admissible native item" || test.name == "known native label refusal" {
				claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "candidate-parity", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: current.WorkflowStates, LabelExclude: current.LabelExclude}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
				status := http.StatusOK
				if test.code != "" {
					status = http.StatusConflict
				}
				requireNativeStatus(t, response, status)
				if test.code != "" && !strings.Contains(response.Body.String(), `"code":"no_claimable_work"`) {
					t.Fatalf("explanation disagrees with claim refusal: %s", response.Body)
				}
			}
		})
	}
}
