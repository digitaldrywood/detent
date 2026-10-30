package hubserver

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubhealth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
			window := operatortool.AnalyticsWindow{From: now.Add(-hubhealth.HistoryWindow), To: now}
			read := func() []hubhealth.Finding {
				t.Helper()
				attempts, err := readHealthRateAttempts(t.Context(), f.service.database.reader, scope, window)
				if err != nil {
					t.Fatal(err)
				}
				if err := readHealthProductivityAttempts(t.Context(), f.service.database.reader, scope, attempts); err != nil {
					t.Fatal(err)
				}
				return hubhealth.ProductivityFindings(now, hubhealth.RateProject{ID: string(f.project.ID), Attempts: attempts})
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
			if len(findings) != 2 || slices.ContainsFunc(findings, func(f hubhealth.Finding) bool { return f.Subject.ID == string(items[0].WorkItemID) }) {
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
