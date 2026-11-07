package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type postgresShutdownScheduling struct {
	repository *postgres.Repository
	org        tracker.OrganizationID
	issue      connector.Issue
	lease      leasestore.Lease
	exited     <-chan struct{}
}

func (s *postgresShutdownScheduling) HeartbeatInterval() time.Duration { return time.Hour }

func (s *postgresShutdownScheduling) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	return []connector.Issue{s.issue}, nil
}

func (s *postgresShutdownScheduling) AdoptClaim(_ context.Context, issue connector.Issue, now time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{Issue: issue, ClaimedAt: now, Owner: s.lease.MachineID, LeaseRenewedAt: s.lease.RenewedAt, LeaseExpiresAt: s.lease.ExpiresAt}, nil
}

func (s *postgresShutdownScheduling) RenewClaim(context.Context, string, time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{}, nil
}

func (s *postgresShutdownScheduling) ReleaseClaim(ctx context.Context, _ string, reason string) error {
	select {
	case <-s.exited:
	default:
		return errors.New("lease released before worker exit")
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("shutdown release has no deadline")
	}
	return s.repository.WriteLeases(ctx, s.org, corestore.Serving, func(w leasestore.Writer) error {
		_, err := w.Release(ctx, leaseFence(s.lease), reason)
		return err
	})
}

type postgresShutdownConnector struct{}

func (postgresShutdownConnector) Name() string { return "github" }
func (postgresShutdownConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}
func (postgresShutdownConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}
func (postgresShutdownConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}
func (postgresShutdownConnector) CreateComment(context.Context, string, string) error { return nil }
func (postgresShutdownConnector) UpdateIssueState(context.Context, string, string) error {
	return nil
}
func (postgresShutdownConnector) SetAssignee(context.Context, string, string) error { return nil }
func (postgresShutdownConnector) SetField(context.Context, string, string, string) error {
	return nil
}

type postgresShutdownRunner struct {
	started chan struct{}
	exited  chan struct{}
}

func (r *postgresShutdownRunner) Run(ctx context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	defer close(r.exited)
	close(r.started)
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}

func TestRunnerShutdownReleasesPostgresLeaseBeforeExpiry(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	leases := make(map[tracker.OrganizationID]leasestore.Lease)
	for _, org := range organizations {
		seedLeaseFamily(t, repository, org)
		leases[org] = claimLease(t, repository, org, leaseClaim(1))
	}
	for _, org := range organizations {
		t.Run(string(org), func(t *testing.T) {
			backend := &postgresShutdownRunner{started: make(chan struct{}), exited: make(chan struct{})}
			issue := connector.NewIssue()
			issue.ID, issue.Identifier, issue.Title, issue.State = "1", "example-org/example-project#1", "Example", "Todo"
			issue.Fields["detent_hub_work_item_id"] = "1"
			source := &postgresShutdownScheduling{repository: repository, org: org, issue: issue, lease: leases[org], exited: backend.exited}
			orch, err := orchestrator.New(orchestrator.Config{Project: scheduler.ProjectCandidate{ID: "prj_1"}, PollInterval: time.Hour, MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, SchedulingRepository: "example-org/example-project"}, orchestrator.Dependencies{Connector: postgresShutdownConnector{}, Scheduling: source, Runner: backend})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			var runErr error
			go func() { defer close(done); runErr = orch.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			select {
			case <-backend.started:
			case <-done:
				t.Fatalf("orchestrator stopped before dispatch: %v", runErr)
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("worker did not start")
			}
			cancel()
			<-done
			<-backend.exited
			if !errors.Is(runErr, context.Canceled) {
				t.Fatalf("shutdown: %v", runErr)
			}
			err = repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
				lease, err := r.Lease(t.Context(), source.lease.LeaseID)
				if err != nil {
					return err
				}
				if lease.ReleasedAt == nil || !lease.ReleasedAt.Before(lease.ExpiresAt) {
					return fmt.Errorf("shutdown left lease held until expiry: %+v", lease)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			replacement := leaseClaim(1)
			replacement.LeaseID, replacement.SessionID = "lease_replacement", "session_replacement"
			claimLease(t, repository, org, replacement)
			if org == organizations[0] {
				err = repository.ReadLeases(t.Context(), organizations[1], corestore.Serving, func(r leasestore.Reader) error {
					lease, err := r.ActiveLeaseForIssue(t.Context(), 1)
					if err == nil && lease.LeaseID != leases[organizations[1]].LeaseID {
						return errors.New("shutdown changed the other tenant's lease")
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
