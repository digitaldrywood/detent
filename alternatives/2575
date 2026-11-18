package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ordinaryProcessRepository struct{ *postgres.Repository }

func (r ordinaryProcessRepository) WriteRunnerEventCommand(ctx context.Context, org tracker.OrganizationID, _, _ string, _ eventstore.RunCommand, write func(context.Context, tenantstore.Writer) error) error {
	return r.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error { return write(ctx, w) })
}

func TestWorkerSessionMutationPublishesProcessReceiptAtomically(t *testing.T) {
	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprintf("ordinary_%t", ordinary), func(t *testing.T) {
			first, second, repository, _, _ := newRunnerEventWireFixtures(t)
			if _, err := first.admin.ExecContext(t.Context(), `CREATE FUNCTION reject_worker_receipt() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.pid=9999 THEN RAISE EXCEPTION 'fixture process receipt rejection'; END IF; RETURN NEW; END$$; CREATE TRIGGER reject_worker_receipt BEFORE INSERT ON runner_process_receipts FOR EACH ROW EXECUTE FUNCTION reject_worker_receipt()`); err != nil {
				t.Fatal(err)
			}
			for tenant, fixture := range []runnerPGFixture{first, second} {
				org := tracker.OrganizationID("org_startup")
				if tenant == 1 {
					org = "org_second"
				}
				if ordinary {
					fixture.service.config.TenantRepository = ordinaryProcessRepository{repository}
				}
				for index, lease := range fixture.leases {
					path := fmt.Sprintf("/api/v2/organizations/%s/projects/prj_runner/runner-sessions", org)
					mutate := func(request runnersession.Mutation, status int) runnersession.Snapshot {
						t.Helper()
						response := performHubAPIRequest(t, fixture.service, http.MethodPost, path, fixture.tokens[index], request)
						requireNativeStatus(t, response, status)
						var snapshot runnersession.Snapshot
						if status == http.StatusOK {
							decodeHubResponse(t, response, &snapshot)
						}
						return snapshot
					}
					key := strings.Repeat("x", 32)
					started := time.Now().UTC().Truncate(time.Microsecond).Add(123 * time.Nanosecond)
					request := runnersession.Mutation{Key: fmt.Sprintf("start_%d", index), LeaseID: lease.ID, FencingToken: lease.FencingToken, ProcessKey: key, Start: &store.SessionStart{IssueID: string(lease.WorkItemID), StartedAt: started, Model: "example-model"}}
					snapshot := mutate(request, http.StatusOK)
					registration := store.WorkerProcessRegistration{CleanupRoot: "/example/workspaces", CleanupPath: "/example/workspaces/job"}
					request.Start, request.Worker = nil, &registration
					request.SessionID, request.ExpectedRevision, request.Key = snapshot.ID, snapshot.Revision, "reserve"
					snapshot = mutate(request, http.StatusOK)
					request.ExpectedRevision, request.Key = snapshot.Revision, "worker"
					registration.WorkerProcessIdentity = store.WorkerProcessIdentity{PID: 9999, GroupID: 9999, StartedAt: started}
					mutate(request, http.StatusInternalServerError)
					if err := repository.ReadOperatorProcesses(t.Context(), org, corestore.Serving, func(r postgres.OperatorProcessReader) error {
						current, err := r.RunnerSession(t.Context(), "prj_runner", snapshot.ID)
						if err != nil {
							return err
						}
						if current.Revision != snapshot.Revision || current.WorkerPID != 0 {
							t.Fatalf("receipt rejection committed session state: %+v", current)
						}
						history, err := r.RunnerSessionHistory(t.Context(), "prj_runner", snapshot.ID, snapshot.Revision, 10)
						if err == nil && len(history) != 0 {
							t.Fatal("receipt rejection committed worker history")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					registration.WorkerProcessIdentity = store.WorkerProcessIdentity{PID: 4000 + index, GroupID: 4000 + index, StartedAt: started}
					snapshot = mutate(request, http.StatusOK)
					var receipt postgres.OperatorProcess
					if err := repository.ReadOperatorProcesses(t.Context(), org, corestore.Serving, func(r postgres.OperatorProcessReader) error {
						binding := fixture.bindings[index]
						rows, err := r.ListActiveWorkerProcesses(t.Context(), postgres.OperatorProcessList{OrganizationID: org, ProjectID: "prj_runner", RunnerID: binding.RunnerID, EnrollmentID: fmt.Sprintf("enrollment_%d", index+1), MachineID: string(binding.MachineID), Now: time.Now().UTC(), PageLimit: 10})
						if err != nil {
							return err
						}
						if len(rows) != 1 {
							t.Fatalf("normal worker mutation published %d receipts", len(rows))
						}
						receipt = rows[0]
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if receipt.SessionID != snapshot.ID || receipt.Pid != int32(registration.PID) || receipt.StartedAtNs != started.UnixNano() || receipt.HistoryRevision != snapshot.Revision || receipt.AuthorityHash != runnerSessionProcessAuthority(key) || receipt.FencingToken != int64(lease.FencingToken) || receipt.OwnerGeneration != 1 {
						t.Fatalf("receipt lost fenced worker identity: %+v", receipt)
					}
					var history runnersession.Mutation
					ref := eventstore.Reference(receipt.RegistrationObjectKey, receipt.RegistrationSha256, receipt.RegistrationBytes, receipt.RegistrationContentBytes)
					raw, err := fixture.service.runDetailStore().ReadDetail(t.Context(), string(org), ref)
					if err != nil || json.Unmarshal(raw, &history) != nil || history.Worker == nil || history.Worker.PID != registration.PID || !history.Worker.StartedAt.Equal(started) {
						t.Fatalf("receipt does not reference verified worker history: %v", err)
					}
					request.ExpectedRevision, request.Key = snapshot.Revision, "replacement"
					registration.PID += 100
					snapshot = mutate(request, http.StatusOK)
					var generation, revision int64
					if err := first.admin.QueryRowContext(t.Context(), `SELECT owner_generation,history_revision FROM runner_process_receipts WHERE organization_id=$1 AND project_id='prj_runner' AND session_id=$2 AND role='agent'`, org, snapshot.ID).Scan(&generation, &revision); err != nil {
						t.Fatal(err)
					}
					if generation != 2 || revision != snapshot.Revision {
						t.Fatalf("replacement did not fence the old process receipt: generation=%d history=%d", generation, revision)
					}
					request.ExpectedRevision, request.Key, request.ProcessKey = snapshot.Revision, "foreign", strings.Repeat("y", 32)
					mutate(request, http.StatusUnauthorized)
				}
			}
		})
	}
}
