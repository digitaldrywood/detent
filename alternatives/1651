package hubserver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthProductivityFindings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*healthRateProject)
		want   []string
	}{
		{name: "repeated failures", want: []string{"activity_without_delivery", "unproductive_attempts"}},
		{name: "landing clears project", mutate: func(p *healthRateProject) {
			p.Attempts = append(p.Attempts, healthRateAttempt{Landed: true, LandingAt: now})
		}, want: []string{"unproductive_attempts"}},
		{name: "Done clears project", mutate: func(p *healthRateProject) {
			p.Timelines = []analyticsResidenceTimeline{{done: []time.Time{now.Add(-time.Minute)}}}
		}, want: []string{"unproductive_attempts"}},
		{name: "landing before window", mutate: func(p *healthRateProject) {
			p.Attempts = append(p.Attempts, healthRateAttempt{Landed: true, LandingAt: now.Add(-30*time.Minute - time.Nanosecond)})
		}, want: []string{"activity_without_delivery", "unproductive_attempts"}},
		{name: "landing at window start", mutate: func(p *healthRateProject) {
			p.Attempts = append(p.Attempts, healthRateAttempt{Landed: true, LandingAt: now.Add(-30 * time.Minute)})
		}, want: []string{"unproductive_attempts"}},
		{name: "changed signature breaks streak", mutate: func(p *healthRateProject) {
			p.Attempts[4].Signature = "different failure"
		}, want: []string{"activity_without_delivery"}},
		{name: "changed stage breaks streak", mutate: func(p *healthRateProject) {
			p.Attempts[4].Stage = "event_publication"
		}, want: []string{"activity_without_delivery"}},
		{name: "verified source clears streak", mutate: func(p *healthRateProject) {
			p.Attempts[4].SourceVersion = "version-new"
		}, want: []string{"activity_without_delivery"}},
		{name: "replayed source does not clear streak", mutate: func(p *healthRateProject) {
			first := p.Attempts[0]
			first.SourceVersion = "version-original"
			p.Attempts = append([]healthRateAttempt{first}, p.Attempts...)
			for i := range p.Attempts {
				p.Attempts[i].SourceVersion = "version-original"
			}
		}, want: []string{"activity_without_delivery", "unproductive_attempts"}},
		{name: "private stage is omitted", mutate: func(p *healthRateProject) {
			p.Attempts[4].Stage = "private-customer-content"
		}, want: []string{"activity_without_delivery"}},
		{name: "successful attempt breaks streak", mutate: func(p *healthRateProject) {
			p.Attempts[4].Status, p.Attempts[4].Signature = "succeeded", ""
		}, want: []string{"activity_without_delivery"}},
		{name: "below repetition threshold", mutate: func(p *healthRateProject) {
			p.Attempts = p.Attempts[:4]
		}},
		{name: "running attempt spans window with no free slots", mutate: func(p *healthRateProject) {
			p.Attempts = p.Attempts[:1]
			p.Attempts[0].Status, p.Attempts[0].Signature = "running", ""
			p.Attempts[0].Started = now.Add(-30 * time.Minute)
		}, want: []string{"activity_without_delivery"}},
		{name: "new running attempt", mutate: func(p *healthRateProject) {
			p.Attempts = p.Attempts[:1]
			p.Attempts[0].Status, p.Attempts[0].Signature = "running", ""
		}},
		{name: "partial residence avoids false project finding", mutate: func(p *healthRateProject) {
			p.ResidencePartial = true
		}, want: []string{"unproductive_attempts"}},
		{name: "future attempts excluded", mutate: func(p *healthRateProject) {
			p.Attempts[4].At = now.Add(time.Second)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := healthRateProject{ID: "project"}
			for i := range 5 {
				p.Attempts = append(p.Attempts, healthRateAttempt{ID: fmt.Sprint("attempt-", i), Item: "item", Status: "failed", Session: fmt.Sprint("session-", i), Stage: "finalization", Signature: "private-customer-content", Started: now.Add(-time.Duration(10-i)*time.Minute - time.Second), At: now.Add(-time.Duration(10-i) * time.Minute)})
			}
			if test.mutate != nil {
				test.mutate(&p)
			}
			before, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			got := evaluateHealth(now, healthSnapshot{Rates: []healthRateProject{p}})
			signals := []string{}
			for _, finding := range got {
				if finding.Signal != "activity_without_delivery" && finding.Signal != "unproductive_attempts" {
					continue
				}
				signals = append(signals, finding.Signal)
				if finding.Class != "human" || finding.Severity != "attention" || finding.Evidence.Productivity == nil || finding.Evidence.Productivity.WindowFrom.IsZero() || finding.Evidence.Productivity.WindowTo.IsZero() || finding.Evidence.Productivity.LatestFailureStage == "" {
					t.Fatalf("incomplete observation: %+v", finding)
				}
				if finding.Signal == "unproductive_attempts" && (finding.Evidence.Counts["consecutive_unproductive_attempts"] != 5 || len(finding.Evidence.AttemptIDs) != 5 || finding.Evidence.Productivity.LatestFailureStage != "finalization") {
					t.Fatalf("streak evidence = %+v", finding.Evidence)
				}
				if finding.Signal == "activity_without_delivery" && len(p.Attempts) == 5 && (finding.Evidence.Counts["attempts"] != 5 || finding.Evidence.Counts["sessions"] != 5) {
					t.Fatalf("activity evidence = %+v", finding.Evidence)
				}
				raw, err := json.Marshal(finding)
				if err != nil || strings.Contains(string(raw), "private-customer-content") {
					t.Fatalf("unsafe finding: %s error=%v", raw, err)
				}
			}
			slices.Sort(signals)
			if !slices.Equal(signals, test.want) {
				t.Fatalf("signals=%v want=%v", signals, test.want)
			}
			after, err := json.Marshal(p)
			if err != nil || string(before) != string(after) {
				t.Fatalf("evaluation changed inputs: error=%v", err)
			}
		})
	}
}

func TestHealthProductivityPersistedObservations(t *testing.T) {
	t.Parallel()
	for _, projected := range []bool{false, true} {
		t.Run(fmt.Sprint("projected=", projected), func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "productivity")
			addHealthBacklog(t, f)
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
			now := time.Now().UTC().Add(time.Minute)
			items := []tracker.NativeIssue{f.create(t, "first item"), f.create(t, "second item")}
			query := claimCandidateQuery{NativeScope: &scope, AvailableAt: now, Limit: 20, PageSize: 20}
			before, err := nativeCandidateIDs(t.Context(), f.service.database.reader, query, nil, nil, nil, nil, nil, nil, nil)
			if err != nil || len(before) != 2 {
				t.Fatalf("candidates before=%v error=%v", before, err)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, item := range items {
				for i := range 5 {
					at := now.Add(-time.Duration(10-i) * time.Minute)
					data := tracker.NativeRunData{SessionID: fmt.Sprint(item.WorkItemID, "-session-", i), TerminalFailure: &tracker.NativeTerminalFailure{Stage: "finalization", Error: "private-customer-content"}}
					id := seedHealthRateAttempt(t, tx, scope, item, string(r.binding.MachineID), "failed", data, at, -1)
					if projected {
						if err := updateAttemptHotProjection(t.Context(), tx, id, "failed", data); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			window := operatortool.AnalyticsWindow{From: now.Add(-healthHistoryWindow), To: now}
			read := func() []healthFinding {
				t.Helper()
				attempts, err := readHealthRateAttempts(t.Context(), f.service.database.reader, scope, window)
				if err != nil {
					t.Fatal(err)
				}
				if err := readHealthProductivityAttempts(t.Context(), f.service.database.reader, scope, attempts); err != nil {
					t.Fatal(err)
				}
				return healthProductivityFindings(now, healthRateProject{ID: string(f.project.ID), Attempts: attempts})
			}
			findings := read()
			if len(findings) != 3 {
				t.Fatalf("findings=%+v", findings)
			}
			for _, finding := range findings {
				if finding.Signal == "activity_without_delivery" && (finding.Evidence.Counts["attempts"] != 10 || finding.Evidence.Counts["sessions"] != 10) {
					t.Fatalf("project evidence=%+v", finding.Evidence)
				}
			}
			applyTestHealth(t, f, now, findings)
			after, err := nativeCandidateIDs(t.Context(), f.service.database.reader, query, nil, nil, nil, nil, nil, nil, nil)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("dispatch candidates changed: before=%v after=%v error=%v", before, after, err)
			}
			tx, err = f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			data := tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Stage: "finalization", Error: "private-customer-content"}, Finalization: &tracker.NativeFinalization{Settled: true, Changed: true, VersionID: "new-version"}}
			id := seedHealthRateAttempt(t, tx, scope, items[0], string(r.binding.MachineID), "failed", data, now.Add(-time.Minute), -1)
			if projected {
				if err := updateAttemptHotProjection(t.Context(), tx, id, "failed", data); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			findings = read()
			if len(findings) != 2 || slices.ContainsFunc(findings, func(f healthFinding) bool { return f.Subject.ID == string(items[0].WorkItemID) }) {
				t.Fatalf("source change did not clear its item's finding: %+v", findings)
			}
			applyTestHealth(t, f, now.Add(time.Second), findings)
			var resolved int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM health_findings WHERE signal='unproductive_attempts' AND resolved_at IS NOT NULL").Scan(&resolved); err != nil || resolved != 1 {
				t.Fatalf("resolved=%d error=%v", resolved, err)
			}
		})
	}
}
