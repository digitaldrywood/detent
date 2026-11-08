package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOperatorProcessContractFencesDurableReaping(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	objects := newHistoryObjects()
	var owners []postgres.ProcessReceipt
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedLeaseFamily(t, repository, org)
		for id := int64(1); id <= 2; id++ {
			runner := fmt.Sprintf("runner_%d", id)
			machine := fmt.Sprintf("machine_%d", id)
			seedRunnerIdentity(t, repository, org, runner, machine, time.Now().UTC(), false)
			lease := claimLease(t, repository, org, leaseClaim(id))
			event := attemptEvent(lease)
			if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error { _, err := w.RecordAttempt(t.Context(), event); return err }); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: runner, LeaseID: lease.LeaseID, AssignedAt: lease.AcquiredAt})
			}); err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(org), []byte(fmt.Sprintf("session_%d", id)))
			if err != nil {
				t.Fatal(err)
			}
			at := lease.AcquiredAt.Add(time.Second)
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			var session runnersession.Record
			if err := repository.WriteOperatorProcesses(t.Context(), org, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
				var err error
				session, err = w.CreateRunnerSession(t.Context(), runnersession.Record{Organization: org, Project: project, IssueID: fmt.Sprintf("wi_%d", id), AttemptID: event.ID, StartedAt: lease.AcquiredAt, Reference: ref}, runnersession.History{Key: fmt.Sprintf("start_%d", id), Reference: ref})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			started := lease.AcquiredAt.Add(123 * time.Nanosecond)
			row := postgres.ProcessReceipt{OrganizationID: org, ProjectID: project, SessionID: session.ID, Role: "agent", RunnerID: runner, EnrollmentID: "enrollment_" + runner, MachineID: machine, LeaseID: string(lease.LeaseID), FencingToken: int64(lease.FencingToken), AuthorityHash: artifact.Digest([]byte("authority")), Pid: int32(100 + id), GroupID: int32(100 + id), StartedAt: started, HistoryRevision: 1, RegistrationObjectKey: ref.Key, RegistrationSha256: ref.SHA256, RegistrationBytes: ref.CompressedSize, RegistrationContentBytes: ref.UncompressedSize}
			register := func(value postgres.ProcessReceipt) (postgres.ProcessReceipt, error) {
				var result postgres.ProcessReceipt
				err := repository.WriteOperatorProcesses(t.Context(), org, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
					var err error
					result, err = w.RegisterWorkerProcess(t.Context(), value, at)
					return err
				})
				return result, err
			}
			for _, tc := range []struct {
				name   string
				change func(*postgres.ProcessReceipt)
			}{
				{"fence", func(r *postgres.ProcessReceipt) { r.FencingToken++ }},
				{"runner", func(r *postgres.ProcessReceipt) { r.RunnerID = "other" }},
				{"history", func(r *postgres.ProcessReceipt) { r.RegistrationSha256 = artifact.Digest([]byte("other")) }},
				{"tenant", func(r *postgres.ProcessReceipt) { r.OrganizationID = "foreign" }},
			} {
				changed := row
				tc.change(&changed)
				if _, err := register(changed); err == nil {
					t.Fatalf("%s registration accepted", tc.name)
				}
			}
			registered, err := register(row)
			if err != nil {
				t.Fatal(err)
			}
			if registered.OwnerGeneration != 1 || registered.StartedAtNs != started.UnixNano() {
				t.Fatalf("registered=%+v", registered)
			}
			replay, err := register(row)
			if err != nil || replay.OwnerGeneration != 1 {
				t.Fatalf("registration replay=%+v: %v", replay, err)
			}
			changed := row
			changed.Pid++
			if _, err := register(changed); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("live identity overwritten: %v", err)
			}
			reapRef, err := objects.StoreDetail(t.Context(), string(org), []byte(fmt.Sprintf("reap_%d", id)))
			if err != nil {
				t.Fatal(err)
			}
			reap := postgres.ProcessReap{Owner: registered, Classification: "committed", Outcome: "exited", At: at, Receipt: reapRef}
			if err := repository.WriteOperatorProcesses(t.Context(), org, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
				_, err := w.MarkSessionWorkerProcessReaped(t.Context(), reap)
				return err
			}); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("live execution reaped: %v", err)
			}
			if err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), leaseFence(lease), "complete")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name   string
				change func(*postgres.ProcessReceipt)
			}{
				{"generation", func(r *postgres.ProcessReceipt) { r.OwnerGeneration++ }},
				{"pid", func(r *postgres.ProcessReceipt) { r.Pid++ }},
				{"nanosecond identity", func(r *postgres.ProcessReceipt) { r.StartedAtNs++ }},
				{"authority", func(r *postgres.ProcessReceipt) { r.AuthorityHash = artifact.Digest([]byte("other")) }},
			} {
				changed := reap
				tc.change(&changed.Owner)
				if err := repository.WriteOperatorProcesses(t.Context(), org, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
					_, err := w.MarkSessionWorkerProcessReaped(t.Context(), changed)
					return err
				}); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("%s reap accepted: %v", tc.name, err)
				}
			}
			reap.Classification = "uncertain"
			if err := repository.WriteOperatorProcesses(t.Context(), org, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
				_, err := w.MarkSessionWorkerProcessReaped(t.Context(), reap)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			owners = append(owners, registered)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := repositoryFor(t, admin, smallPool())
	for _, owner := range owners {
		input := postgres.OperatorProcessList{OrganizationID: owner.OrganizationID, ProjectID: owner.ProjectID, RunnerID: owner.RunnerID, EnrollmentID: owner.EnrollmentID, MachineID: owner.MachineID, Now: time.Now().UTC(), PageLimit: 2}
		if err := reopened.ReadOperatorProcesses(t.Context(), owner.OrganizationID, corestore.Serving, func(r postgres.OperatorProcessReader) error {
			rows, err := r.ListActiveWorkerProcesses(t.Context(), input)
			if err != nil {
				return err
			}
			if len(rows) != 1 || !rows[0].SafeToReap.Valid || !rows[0].SafeToReap.Bool || rows[0].ReapClass != "uncertain" || rows[0].StartedAtNs != owner.StartedAtNs {
				t.Fatalf("durable uncertain processes=%+v", rows)
			}
			wrong := input
			wrong.EnrollmentID = "other"
			rows, err = r.ListActiveWorkerProcesses(t.Context(), wrong)
			if err == nil && len(rows) != 0 {
				t.Fatalf("foreign enrollment read=%+v", rows)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ref, err := objects.StoreDetail(t.Context(), string(owner.OrganizationID), []byte("confirmed"))
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := reopened.WriteOperatorProcesses(t.Context(), owner.OrganizationID, corestore.Serving, func(w postgres.OperatorProcessWriter) error {
				receipt, err := w.MarkSessionWorkerProcessReaped(t.Context(), postgres.ProcessReap{Owner: owner, Classification: "committed", Outcome: "exited", At: time.Now().UTC(), Receipt: ref})
				if err == nil && receipt.ReapedAt == nil {
					t.Fatal("definite reap missing")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := reopened.ReadOperatorProcesses(t.Context(), owner.OrganizationID, corestore.Serving, func(r postgres.OperatorProcessReader) error {
			rows, err := r.ListActiveWorkerProcesses(t.Context(), input)
			if err == nil && len(rows) != 0 {
				t.Fatalf("definite reap still active=%+v", rows)
			}
			session, readErr := r.RunnerSession(t.Context(), owner.ProjectID, owner.SessionID)
			if readErr != nil {
				return readErr
			}
			if session.WorkerReapedAt == nil {
				t.Fatal("session projection missing reap")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
