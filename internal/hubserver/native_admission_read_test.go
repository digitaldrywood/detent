package hubserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
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
		name               string
		label              string
		body               string
		dependency         bool
		extraDependencies  int
		ignoreDependencies bool
		privateDependency  bool
		intake             bool
		context            bool
		heartbeat          bool
		another            bool
		observation        string
		policy             string
		provider           string
		stale              bool
		oldVersion         bool
		outcome            string
		code               string
		unavailable        string
		authority          string
		status             int
	}{
		{name: "stored observation cannot survive revoked credential", heartbeat: true, authority: "revoked", status: http.StatusUnauthorized},
		{name: "stored observation cannot survive removed grant", heartbeat: true, authority: "grant", status: http.StatusNotFound},
		{name: "another runner can admit after first runner selector refusal", heartbeat: true, another: true, label: "excluded", outcome: "skipped", code: "no_claimable_work"},
		{name: "hosted heartbeat selector refusal", heartbeat: true, label: "excluded", outcome: "skipped", code: "no_claimable_work"},
		{name: "hosted heartbeat admissible", heartbeat: true, outcome: "ready"},
		{name: "unrelated heartbeat cannot freshen selectors", heartbeat: true, observation: "stale", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "routing change cannot restamp selector revision", heartbeat: true, observation: "revision", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "stored old policy is unknown", heartbeat: true, observation: "policy", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "future selector observation is unknown", heartbeat: true, observation: "future", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "another project observation is not inherited", heartbeat: true, observation: "project", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "known native label refusal", label: "excluded", context: true, outcome: "skipped", code: "no_claimable_work"},
		{name: "admissible native item", label: "ordinary", context: true, outcome: "ready"},
		{name: "ordinary label alone proves no refusal", label: "ordinary", outcome: "unknown", unavailable: "runner_specific_candidate_selection"},
		{name: "human owned label refuses before missing provider requirement", label: "human-owned", heartbeat: true, provider: "unknown", outcome: "skipped", code: "inactive_state"},
		{name: "hosted unfinished dependency refuses", heartbeat: true, dependency: true, outcome: "skipped", code: "no_claimable_work"},
		{name: "multiple unfinished dependencies retain evidence", heartbeat: true, dependency: true, extraDependencies: 1, outcome: "skipped", code: "no_claimable_work"},
		{name: "dependency evidence remains bounded", heartbeat: true, dependency: true, extraDependencies: 100, outcome: "skipped", code: "no_claimable_work", unavailable: "unresolved_dependencies"},
		{name: "dependency and intake retain unknown other exclusions", heartbeat: true, dependency: true, intake: true, outcome: "skipped", code: "no_claimable_work"},
		{name: "optional unfinished dependency permits candidate", heartbeat: true, dependency: true, ignoreDependencies: true, outcome: "ready"},
		{name: "optional dependency cannot explain intake refusal", heartbeat: true, dependency: true, ignoreDependencies: true, intake: true, outcome: "skipped", code: "no_claimable_work", unavailable: "native_candidate_exclusion"},
		{name: "private dependency retains generic refusal", heartbeat: true, dependency: true, privateDependency: true, outcome: "skipped", code: "no_claimable_work", unavailable: "native_candidate_exclusion"},
		{name: "typed human task refuses without label or selectors", body: "```detent-human\nschema: 1\nkey: operator-task\naction: Record measured costs\nowner: operator\ncompletion_criteria: Provide measurement evidence\napproval_constraint: No purchases authorized\n```", outcome: "skipped", code: "inactive_state"},
		{name: "tracking epic refuses", label: "epic", context: true, outcome: "skipped", code: "inactive_state"},
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
			var other runnerFixture
			if test.another {
				other = prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				if other.binding.RunnerID < r.binding.RunnerID {
					r, other = other, r
				}
			}
			r.enroll(t)
			issue := f.create(t, "native candidate")
			var blockers []tracker.NativeIssue
			if test.dependency {
				blockerFixture := f
				if test.privateDependency {
					blockerFixture = newNativeFixture(t, f.service, f.project.OrganizationID, "private-dependency")
				}
				for index := range 1 + test.extraDependencies {
					blocker := blockerFixture.create(t, "unfinished blocker "+strconv.Itoa(index))
					blockers = append(blockers, blocker)
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO issue_dependencies (dependent_issue_id, blocker_issue_id, provenance, created_at, updated_at) SELECT a.id, b.id, 'native', ?, ? FROM issues a, issues b WHERE a.native_id = ? AND b.native_id = ?", testTimestamp, testTimestamp, issue.WorkItemID, blocker.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.ignoreDependencies {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_dependencies=0 WHERE id=?", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.intake {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO github_imports (id, project_id, issue_number, work_item_id, intake_pending, observed_at) VALUES (?, ?, 1, ?, 1, ?)", newNativeID("import"), f.project.ID, issue.WorkItemID, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			labelValues := []string{}
			if test.label != "" {
				labelValues = append(labelValues, test.label)
			}
			labels, err := json.Marshal(labelValues)
			if err != nil {
				t.Fatal(err)
			}
			body := "private-body /private/runner credential-sentinel"
			if test.body != "" {
				body += "\n" + test.body
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json=?, body=? WHERE native_id=?", string(labels), body, issue.WorkItemID); err != nil {
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
			current := tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: descriptor.ID, WorkflowStates: []string{"Todo"}, LabelExclude: []string{"excluded"}}
			if test.policy == "stale" {
				current.PolicyID = "policy_stale"
			}
			if test.authority == "oversized" {
				current.LabelExclude = []string{strings.Repeat("x", 8192)}
			}
			if test.heartbeat {
				before := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
				requireNativeStatus(t, before, http.StatusOK)
				var baseline tracker.NativeRuntimeEvidence
				decodeHubResponse(t, before, &baseline)
				baselineOutcome := "unknown"
				if test.label == "human-owned" || (test.dependency && !test.ignoreDependencies) || test.intake {
					baselineOutcome = "skipped"
				}
				if baseline.Scheduling.Outcome != baselineOutcome || len(baseline.Admission) != 1 || baseline.Admission[0].SelectorObservedAt != nil {
					t.Fatalf("unknown selectors were invented: %#v", baseline)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capabilities_json=json_set(capabilities_json, '$.sprite_name', 'preserved') WHERE id=?", r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				context := current
				switch test.observation {
				case "stale":
					context.ObservedAt = now.Add(-runnerauth.HeartbeatTimeout)
				case "policy":
					context.PolicyID = "policy_old"
				case "future":
					context.ObservedAt = now.Add(time.Second)
				}
				payload := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "admission": map[string]any{"context": context, "runner_revision": 1}}
				heartbeat := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, payload)
				requireNativeStatus(t, heartbeat, http.StatusOK)
				var preserved string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT json_extract(capabilities_json, '$.sprite_name') FROM machines WHERE id=?", r.binding.MachineID).Scan(&preserved); err != nil || preserved != "preserved" {
					t.Fatalf("heartbeat overwrote existing metadata: %q %v", preserved, err)
				}
				if test.observation == "revision" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET revision=revision+1 WHERE id=?", r.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
				if test.observation == "project" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capabilities_json=json_set(json_remove(capabilities_json, ?), ?, json(?)) WHERE id=?", admissionObservationPath(r.binding.RunnerID, f.project.ID), admissionObservationPath(r.binding.RunnerID, "prj_other"), `{"context":{},"runner_revision":1}`, r.binding.MachineID); err != nil {
						t.Fatal(err)
					}
				}
				if test.another {
					other.enroll(t)
					permitted := current
					permitted.LabelExclude = nil
					otherPayload := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": other.redemption.BackendIsolation, "admission": map[string]any{"context": permitted, "runner_revision": 1}}
					otherHeartbeat := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(other.binding.MachineID)+"/heartbeat", other.redemption.Credential, otherPayload)
					requireNativeStatus(t, otherHeartbeat, http.StatusOK)
				}
				delete(payload, "admission")
				heartbeat = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, payload)
				requireNativeStatus(t, heartbeat, http.StatusOK)
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
				if test.heartbeat {
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/runtime", f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var current tracker.NativeRuntimeEvidence
					decodeHubResponse(t, response, &current)
					for _, admission := range current.Admission {
						if admission.Outcome == "ready" || admission.SelectorObservedAt != nil {
							t.Fatalf("retired authority still supplied selectors: %#v", current)
						}
					}
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			decodeHubResponse(t, response, &evidence)
			if len(evidence.Admission) != 1+map[bool]int{false: 0, true: 1}[test.another] {
				t.Fatalf("admission is unavailable: %#v", evidence)
			}
			a := evidence.Admission[0]
			var expectedDependencies []tracker.NativeDependency
			if test.dependency && !test.ignoreDependencies && !test.privateDependency {
				for _, blocker := range blockers {
					expectedDependencies = append(expectedDependencies, tracker.NativeDependency{Identifier: string(blocker.ProjectID) + "#" + strconv.Itoa(blocker.Number), ID: blocker.WorkItemID, ProjectID: blocker.ProjectID, State: blocker.State, Terminal: false})
				}
				slices.SortFunc(expectedDependencies, func(a, b tracker.NativeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
				expectedDependencies = expectedDependencies[:min(100, len(expectedDependencies))]
			}
			if !reflect.DeepEqual(a.UnresolvedDependencies, expectedDependencies) {
				t.Fatalf("candidate dependency evidence=%#v want=%#v", a.UnresolvedDependencies, expectedDependencies)
			}
			if len(expectedDependencies) > 0 && !slices.Contains(a.Unavailable, "other_native_candidate_exclusions") {
				t.Fatalf("dependency presented as sole exclusion: %#v", a)
			}
			if test.privateDependency && strings.Contains(response.Body.String(), string(blockers[0].WorkItemID)) {
				t.Fatal("private dependency escaped its grant")
			}
			if a.RunnerID != r.binding.RunnerID || a.RunnerRevision < 1 || a.PolicyID != descriptor.ID || a.Outcome != test.outcome || a.ReasonCode != test.code || !a.ObservedAt.Equal(now) || a.Source != "native_claim_candidate_snapshot" {
				t.Fatalf("admission=%#v", a)
			}
			if test.code == "inactive_state" && len(a.Unavailable) != 0 {
				t.Fatalf("known nonexecutable work became provider uncertainty: %#v", a)
			}
			if test.unavailable != "" && !slices.Contains(a.Unavailable, test.unavailable) {
				t.Fatalf("missing unavailable predicate: %#v", a)
			}
			if test.name == "known native label refusal" || test.name == "admissible native item" || test.name == "hosted heartbeat selector refusal" || test.name == "hosted heartbeat admissible" {
				if a.SelectorObservedAt == nil || !a.SelectorObservedAt.Equal(now) || a.SelectorSource != map[bool]string{false: "registered_runner_published_context", true: "registered_runner_heartbeat"}[test.heartbeat] {
					t.Fatalf("selector observation lost its source or freshness: %#v", a)
				}
			}
			explanation := explain.FromNativeEvidence(evidence)
			if test.outcome == "skipped" && (len(explanation.Eligibility.Refusals) == 0 || !reflect.DeepEqual(explanation.Eligibility.Refusals[0].UnresolvedDependencies, expectedDependencies) || !slices.Equal(explanation.Eligibility.Refusals[0].Unavailable, a.Unavailable)) {
				t.Fatalf("explanation dropped current refusal evidence: %#v", explanation.Eligibility)
			}
			if explanation.Eligibility.Latest != nil || evidence.LatestDecision != nil || !slices.Contains(evidence.Unavailable, "historical_scheduler_decision") || explanation.Eligibility.Source != explain.SourceAvailable {
				t.Fatalf("snapshot became history: %#v", explanation.Eligibility)
			}
			if test.another {
				if evidence.Scheduling.Outcome != "ready" || evidence.Admission[1].RunnerID != other.binding.RunnerID || evidence.Admission[1].Outcome != "ready" || evidence.Admission[1].ReasonCode != "" {
					t.Fatalf("one runner refusal poisoned another runner: %#v", evidence)
				}
			} else if test.outcome == "skipped" {
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
				if test.outcome == "skipped" && (len(projected.Eligibility.Refusals) == 0 || !reflect.DeepEqual(projected.Eligibility.Refusals[0].UnresolvedDependencies, expectedDependencies) || !projected.Eligibility.Refusals[0].At.Equal(now) || projected.Eligibility.Refusals[0].Historical || !slices.Equal(projected.Eligibility.Refusals[0].Unavailable, a.Unavailable)) {
					t.Fatalf("MCP dropped current dependency evidence: %s", data)
				}
				if test.privateDependency && strings.Contains(string(data), string(blockers[0].WorkItemID)) {
					t.Fatal("MCP leaked private dependency")
				}
				if test.name == "hosted heartbeat admissible" || test.label == "human-owned" || test.dependency {
					for _, name := range []string{operatortool.Dashboard, operatortool.BoardState} {
						data := hostedContextData(t, call("tools/call", name, map[string]any{"project_id": string(f.project.ID)}), false)
						var board nativeBoardResult
						queued := 1
						if !test.privateDependency {
							queued += len(blockers)
						}
						if err := json.Unmarshal(data, &board); err != nil || board.Counts.QueuedInventory != queued || board.Counts.Running != 0 || board.Counts.ClosedInventory != 0 || board.EligibilityTool != operatortool.ExplainItem || !slices.Contains(board.Unavailable, "aggregate_dispatch_readiness") {
							t.Fatalf("native inventory confused with admission: %s err=%v", data, err)
						}
					}
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
			if test.dependency && !test.ignoreDependencies && !test.intake && !test.privateDependency && test.extraDependencies < 100 {
				for _, blocker := range blockers {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(blocker.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "finish-" + string(blocker.WorkItemID)}, ExpectedRevision: 1, State: "Done", Reason: "user_requested"}), http.StatusOK)
				}
				now = now.Add(time.Second)
				ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
				call := hostedContextProtocol(t, f.service, ctx, "http")
				data := hostedContextData(t, call("tools/call", operatortool.ExplainItem, map[string]any{"project_id": string(f.project.ID), "reference": strconv.Itoa(issue.Number)}), false)
				var restored explain.IssueExplanation
				if err := json.Unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				if restored.Eligibility.State != explain.EligibilityEligible || len(restored.Eligibility.Refusals) != 0 || !restored.ObservedAt.Equal(now) || len(restored.NativeRuntime.Admission[0].UnresolvedDependencies) != 0 {
					t.Fatalf("finished dependencies did not restore current eligibility: %s", data)
				}
			}
			if test.name == "admissible native item" || test.name == "known native label refusal" || (test.dependency && !test.intake && !test.privateDependency && test.extraDependencies < 100) {
				claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "candidate-parity", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: current.WorkflowStates, LabelExclude: current.LabelExclude}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
				status := http.StatusOK
				if test.code != "" && !test.dependency {
					status = http.StatusConflict
				}
				requireNativeStatus(t, response, status)
				if status == http.StatusConflict && !strings.Contains(response.Body.String(), `"code":"no_claimable_work"`) {
					t.Fatalf("explanation disagrees with claim refusal: %s", response.Body)
				}
			}
		})
	}
}
