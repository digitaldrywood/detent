package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthContractQueries struct {
	nativeQueryer
	issues   int
	policies int
}

func (q *healthContractQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.HasPrefix(query, "SELECT o.slug, p.slug, i.id") {
		q.issues++
	}
	if strings.HasPrefix(query, "SELECT r.policy_id, r.metadata_json") {
		q.policies++
	}
	return q.nativeQueryer.QueryRowContext(ctx, query, args...)
}

func TestHealthContractsReuseRevisions(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-contracts")
	f.service.stopHealthDetector()
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-health-contracts")
	first, second := f.create(t, "first"), other.create(t, "second")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	runner.enroll(t)
	routing := fmt.Sprintf(`{"project_ranks":{%q:1}}`, f.project.ID)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET routing_settings_json=?,last_heartbeat_at=?,created_at=? WHERE id=?", routing, formatHubTime(now), formatHubTime(now), runner.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEvent(t, other, second, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "permission_wait"}, now)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.service.database.db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	report, err := json.Marshal(gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: "1", RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/1/attempts/1", Conclusion: "success"})
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO scheduled_validation_reports(organization_id,project_id,repository,run_id,run_attempt,record_json,recorded_at) VALUES(?,?,?,'1','1',?,?)", other.project.OrganizationID, other.project.ID, "example-org/example-project", string(report), formatHubTime(now.Add(-5*time.Hour-29*time.Minute)))
	var cache hubhealth.ContractCache
	for _, test := range []struct {
		name      string
		change    func()
		loads     int
		contracts int
		heartbeat bool
		elapsed   bool
	}{
		{name: "first tick", loads: 2},
		{name: "unchanged next tick"},
		{name: "issue revision invalidates one project", change: func() {
			exec("UPDATE issues SET body='Incomplete contract',revision=revision+1,native_updated_at=? WHERE native_id=?", formatHubTime(now), first.WorkItemID)
		}, loads: 1, contracts: 1},
		{name: "unsatisfied result is reused", contracts: 1},
		{name: "policy revision invalidates one project", change: func() {
			changed := hubTestPolicy()
			changed.SourceRevision = strings.Repeat("b", 40)
			approveHubTestPolicy(t, f.service, f.base+"/policy", changed.WithID())
		}, loads: 1, contracts: 1},
		{name: "heartbeat advances without reevaluation", change: func() { now = now.Add(5 * time.Minute) }, contracts: 1, heartbeat: true},
		{name: "elapsed wait advances without reevaluation", change: func() { now = now.Add(26 * time.Minute) }, contracts: 1, heartbeat: true, elapsed: true},
		{name: "issue correction clears cached finding", change: func() {
			exec("UPDATE issues SET body=?,revision=revision+1,native_updated_at=? WHERE native_id=?", first.Body, formatHubTime(now), first.WorkItemID)
		}, loads: 1, heartbeat: true, elapsed: true},
		{name: "policy removal invalidates", change: func() { clearNativeFixturePolicy(t, other) }, loads: 1, heartbeat: true, elapsed: true},
		{name: "missing policy is reused", heartbeat: true, elapsed: true},
		{name: "policy restoration invalidates", change: func() {
			seedNativeFixturePolicy(t, f.service, string(other.project.OrganizationID)+"/"+string(other.project.ID))
		}, loads: 1, heartbeat: true, elapsed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.change != nil {
				test.change()
			}
			tx, err := f.service.database.reader.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			queries := &healthContractQueries{nativeQueryer: tx}
			snapshot, err := readHealthSnapshotWithContracts(t.Context(), queries, f.project.OrganizationID, clock(), &cache)
			if err != nil {
				t.Fatal(err)
			}
			if queries.issues != test.loads || queries.policies != 2*test.loads {
				t.Fatalf("issue loads=%d contract policy reads=%d, want %d and %d", queries.issues, queries.policies, test.loads, 2*test.loads)
			}
			uncached, err := readHealthSnapshot(t.Context(), tx, f.project.OrganizationID, now)
			if err != nil {
				t.Fatal(err)
			}
			findings, want := hubhealth.Evaluate(now, snapshot), hubhealth.Evaluate(now, uncached)
			sort := func(findings []hubhealth.Finding) {
				slices.SortFunc(findings, func(a, b hubhealth.Finding) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
			}
			sort(findings)
			sort(want)
			if !reflect.DeepEqual(findings, want) {
				t.Fatalf("cached findings=%+v, uncached=%+v", findings, want)
			}
			contracts, heartbeat, elapsed, scheduled := 0, false, false, false
			for _, finding := range findings {
				if finding.Evidence.Reason == "issue_contract_unsatisfied" {
					contracts++
					if finding.Subject.ID != string(first.WorkItemID) || !slices.Equal(finding.Projects, []string{string(f.project.ID)}) {
						t.Fatalf("contract finding escaped project: %+v", finding)
					}
				}
				heartbeat = heartbeat || finding.Signal == "runner_heartbeat_gap"
				elapsed = elapsed || finding.Signal == "unanswered_human_wait" && finding.Subject.ID == string(second.WorkItemID)
				scheduled = scheduled || finding.Signal == "dead_man" && finding.Subject.ID == string(other.project.ID) && finding.Evidence.Counts["scheduled_silent"] == 1
			}
			if contracts != test.contracts || heartbeat != test.heartbeat || elapsed != test.elapsed || scheduled != test.elapsed {
				t.Fatalf("contracts=%d heartbeat=%t elapsed=%t scheduled=%t, want %d %t %t %t", contracts, heartbeat, elapsed, scheduled, test.contracts, test.heartbeat, test.elapsed, test.elapsed)
			}
		})
	}
}

func TestHealthContractsLiveInputs(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"linked intake", "release delegation"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-live")
			f.service.stopHealthDetector()
			issue := f.create(t, "candidate")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := f.service.database.db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			var change func()
			if name == "linked intake" {
				exec("UPDATE issues SET body='Incomplete contract' WHERE native_id=?", issue.WorkItemID)
				exec("INSERT INTO linked_issue_sources(work_item_id,source_url,title_supplied,body_supplied) VALUES(?,?,1,1)", issue.WorkItemID, "https://github.com/example-org/example-project/issues/1")
				change = func() {
					exec("UPDATE linked_issue_sources SET snapshot_json='{}' WHERE work_item_id=?", issue.WorkItemID)
				}
			} else {
				original, err := json.Marshal(issue.IssueContract)
				if err != nil {
					t.Fatal(err)
				}
				exec(`UPDATE issues SET issue_contract_json='{"release_delivery":{"policy_id":"previous","body_sha256":"previous"}}' WHERE native_id=?`, issue.WorkItemID)
				change = func() {
					exec("UPDATE issues SET issue_contract_json=? WHERE native_id=?", string(original), issue.WorkItemID)
				}
			}
			var cache hubhealth.ContractCache
			for step := range 2 {
				if step == 1 {
					change()
				}
				queries := &healthContractQueries{nativeQueryer: f.service.database.reader}
				wait, err := cache.Read(t.Context(), queries, hubhealth.Scope{Organization: scope.organization, Project: scope.project}, string(issue.WorkItemID), issue.Revision, hubTestPolicy().ID, healthContractLoad)
				if err != nil {
					t.Fatal(err)
				}
				wantWait := name == "linked intake" && step == 1 || name == "release delegation" && step == 0
				if queries.issues != 1 || (wait != nil) != wantWait {
					t.Fatalf("step=%d loads=%d wait=%+v, want load and wait=%t", step, queries.issues, wait, wantWait)
				}
			}
		})
	}
}
