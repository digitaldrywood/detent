package hubserver

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestClaimSelectionStaysOffTheWriter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	open := func(t *testing.T) (*Service, int64, int64) {
		t.Helper()
		service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
		if err := service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		repositoryID, first := seedProjection(t, service.database.db)
		if _, err := service.database.db.ExecContext(t.Context(), "UPDATE repositories SET last_reconciled_at = ? WHERE id = ?", formatHubTime(now), repositoryID); err != nil {
			t.Fatal(err)
		}
		inserted, err := service.database.db.ExecContext(t.Context(), "INSERT INTO issues (repository_id, workflow_state_id, github_node_id, github_number, title, url, github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at) SELECT repository_id, workflow_state_id, 'I_second', 2, 'Second', 'https://example.test/2', github_state, source_version, source_updated_at, synchronized_at, created_at, updated_at FROM issues WHERE id = ?", first)
		if err != nil {
			t.Fatal(err)
		}
		second, err := inserted.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return service, first, second
	}
	query := claimCandidateQuery{Repositories: []string{"digitaldrywood/detent"}}
	claim := func(service *Service, machine tracker.MachineID, session string) (tracker.Lease, error) {
		return service.database.claimNext(t.Context(), tracker.ClaimRequest{MachineID: machine, SessionID: session, TTL: time.Minute}, query, time.Hour)
	}

	tests := []struct {
		name string
		run  func(t *testing.T, service *Service, first, second int64)
	}{
		{name: "a refusal with no candidate never takes the writer", run: func(t *testing.T, service *Service, first, second int64) {
			seedHubMachine(t, service, "machine-a", now)
			seedHubMachine(t, service, "machine-b", now)
			seedHubMachine(t, service, "machine-c", now)
			if _, err := claim(service, "machine-a", "session-a"); err != nil {
				t.Fatal(err)
			}
			if _, err := claim(service, "machine-b", "session-b"); err != nil {
				t.Fatal(err)
			}
			var before int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&before); err != nil {
				t.Fatal(err)
			}
			writer, err := service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, claimErr := claim(service, "machine-c", "session-c")
				done <- claimErr
			}()
			select {
			case claimErr := <-done:
				if !errors.Is(claimErr, ErrNoClaimableWork) {
					t.Fatalf("refused claim error = %v, want no claimable work", claimErr)
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("refused claim waited for the held writer")
			}
			if err := writer.Rollback(); err != nil {
				t.Fatal(err)
			}
			var after int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT total_changes()").Scan(&after); err != nil || after != before {
				t.Fatalf("refused claim wrote %d rows, err=%v", after-before, err)
			}
		}},
		{name: "concurrent claims never oversubscribe one host slot", run: func(t *testing.T, service *Service, first, second int64) {
			seedHubMachine(t, service, "machine-a", now)
			const contenders = 8
			var wait sync.WaitGroup
			errs := make(chan error, contenders)
			for index := range contenders {
				wait.Add(1)
				go func() {
					defer wait.Done()
					_, claimErr := claim(service, "machine-a", fmt.Sprintf("session-%d", index))
					errs <- claimErr
				}()
			}
			wait.Wait()
			close(errs)
			won := 0
			for claimErr := range errs {
				if claimErr == nil {
					won++
				} else if !errors.Is(claimErr, ErrNoClaimableWork) {
					t.Fatalf("claim error = %v", claimErr)
				}
			}
			var active int
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM leases WHERE machine_id = 'machine-a' AND released_at IS NULL").Scan(&active); err != nil {
				t.Fatal(err)
			}
			if won != 1 || active != 1 {
				t.Fatalf("winners = %d active leases = %d, want 1 and 1 for a capacity-1 host", won, active)
			}
		}},
		{name: "a selected candidate taken before commit moves to the next one", run: func(t *testing.T, service *Service, first, second int64) {
			seedHubMachine(t, service, "machine-a", now)
			seedHubMachine(t, service, "machine-b", now)
			var revision int64
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT revision FROM issues WHERE id = ?", first).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			taken, err := service.Tracker().Claim(t.Context(), tracker.ClaimRequest{WorkItemID: tracker.WorkItemID(first), MachineID: "machine-a", SessionID: "session-a", TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			_, err = service.database.claimNextOn(t.Context(), service.database.db, nil, claimMode{hint: &claimSelected{id: tracker.WorkItemID(first), revision: revision}}, tracker.ClaimRequest{MachineID: "machine-b", SessionID: "session-b", TTL: time.Minute}, query, time.Hour)
			if !errors.Is(err, errClaimHintStale) {
				t.Fatalf("stale hint error = %v, want stale hint", err)
			}
			lease, err := claim(service, "machine-b", "session-b")
			if err != nil || lease.WorkItemID != tracker.WorkItemID(second) || taken.WorkItemID != tracker.WorkItemID(first) {
				t.Fatalf("claim after stale hint = %+v, %v; want the second work item", lease, err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service, first, second := open(t)
			test.run(t, service, first, second)
		})
	}
}

func TestClaimRetryAfterCommittedDecisionUsesFreshEventIDs(t *testing.T) {
	f := newNativeFixture(t, nil, "", "claim-retry-event-ids")
	runner := prepareRunner(t, f, runnerauth.Claim)
	runner.enroll(t)
	org := f.project.OrganizationID
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.UpdateRunnerCapacityConfig(t.Context(), runner.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.service.config.now()})
	}); err != nil {
		t.Fatal(err)
	}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(runner.redemption.Credential), "", "")
	if err != nil {
		t.Fatalf("runner authentication status=%d error=%v", status, err)
	}
	scope := nativeScope{organization: org, project: f.project.ID, credential: credential}
	first, second, selected := f.create(t, "first decision"), f.create(t, "second decision"), f.create(t, "selected candidate")
	requestFor := func(issue tracker.NativeIssue) tracker.NativeClaim {
		return tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "retry-" + string(issue.WorkItemID), TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
	}
	var firstLeaseID string
	for pass, issues := range [][]tracker.NativeIssue{{first}, {first, second}} {
		for _, issue := range issues {
			request := requestFor(issue)
			lease, err := f.service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
			if err != nil {
				t.Fatalf("pass %d claim: %v", pass, err)
			}
			if issue.WorkItemID == first.WorkItemID {
				if pass == 0 {
					firstLeaseID = lease.LeaseID
				} else if lease.LeaseID != firstLeaseID {
					t.Fatalf("replayed lease = %s, want %s", lease.LeaseID, firstLeaseID)
				}
			}
		}
		request := requestFor(selected)
		now := f.service.config.now()
		stale, err := f.service.readNativeClaimPlan(t.Context(), scope, request, now)
		if err != nil {
			t.Fatal(err)
		}
		stale.candidate.Revision = -1
		if _, err := f.service.claimNativeIssueTenantWithPlan(t.Context(), scope, request, 5*time.Minute, now, stale, nil); !errors.Is(err, ErrNoClaimableWork) {
			t.Fatalf("pass %d: %v, want committed decisions before stale plan refusal", pass, err)
		}
	}
	events := 0
	ids := map[string]bool{}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		for _, issue := range []tracker.NativeIssue{first, second, selected} {
			rows, err := r.Events(t.Context(), f.project.ID, issue.WorkItemID, 0, 100)
			if err != nil {
				return err
			}
			itemEvents := 0
			for _, row := range rows {
				if row.Type != "scheduler.decision" {
					continue
				}
				if issue.WorkItemID == selected.WorkItemID {
					t.Error("stale candidate committed a scheduling decision")
				}
				events++
				itemEvents++
				ids[row.ID] = true
			}
			want := 1
			if issue.WorkItemID == selected.WorkItemID {
				want = 0
			}
			if itemEvents != want {
				t.Errorf("item %s decisions = %d, want %d", issue.WorkItemID, itemEvents, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if events != 2 || len(ids) != 2 {
		t.Fatalf("scheduler decisions=%d distinct ids=%d, want 2 committed events with distinct ids", events, len(ids))
	}
}
