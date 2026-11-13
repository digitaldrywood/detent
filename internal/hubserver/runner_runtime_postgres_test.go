package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerPGFixture struct {
	organization tracker.OrganizationID
	admin        *sql.DB
	bindings     []runnerauth.Binding
	service      *Service
	server       *httptest.Server
	policy       policystore.Revision
	leases       []tracker.NativeLease
	tokens       []string
	data         []tracker.NativeRunData
}

func newRunnerPGFixture(t *testing.T, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	return newRunnerPGFixtureWithDetails(t, nil, customize...)
}

func newRunnerPGFixtureWithDetails(t *testing.T, decorate func(attachment.DetailStore) attachment.DetailStore, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	if decorate != nil {
		cfg.RunDetails = decorate(cfg.RunDetails)
	}
	seedHostedStartup(t, cfg)
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return seedRunnerPGFixture(t, admin, service, "org_startup", customize...)
}

func seedRunnerPGFixture(t *testing.T, admin *sql.DB, service *Service, organization tracker.OrganizationID, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	var err error
	f := runnerPGFixture{organization: organization, admin: admin, service: service, server: httptest.NewServer(service.Handler())}
	t.Cleanup(f.server.Close)
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	descriptor := hubTestPolicy()
	for _, edit := range customize {
		edit(&descriptor)
	}
	policy := descriptor.WithID()
	f.policy, err = eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.ProjectID = "prj_runner"
	f.policy.ApprovedAt, f.policy.ApprovedBy = now, "test_hub_admin"
	err = service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_runner", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.ApproveProjectPolicy(t.Context(), f.policy, ""); err != nil {
			return err
		}
		for index := 1; index <= 2; index++ {
			id := strconv.Itoa(index)
			token, err := apikey.GenerateToken()
			if err != nil {
				return err
			}
			binding := runnerauth.NewBinding()
			f.bindings = append(f.bindings, binding)
			runnerID, machineID, credentialID := binding.RunnerID, string(binding.MachineID), "credential_"+id
			f.tokens = append(f.tokens, token)
			hash := apikey.HashToken(token)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: credentialID, Name: "Example runner " + id, Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credentialID, ProjectID: "prj_runner"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("example_enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_"+id, runnerID, tracker.MachineID(machineID), now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: credentialID, EnrollmentID: "enrollment_" + id, Scope: "projects", State: "active", DisplayName: "Example runner " + id, Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{"prj_runner"}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			state := "Todo"
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_runner", NativeID: tracker.NativeWorkItemID("wi_" + id), Number: int64(index), Title: "Example", WorkflowStateName: &state})
			if err != nil {
				return err
			}
			itemRaw, err := json.Marshal(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: "prj_runner", WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: "Example", State: "Todo"})
			if err != nil {
				return err
			}
			itemVersion, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: "prj_runner", WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: item.Revision}, itemRaw)
			if err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), itemVersion); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machineID, SessionID: "execution_" + id, LeaseID: "lease_" + id, TTL: time.Hour})
			if err != nil {
				return err
			}
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: "prj_runner", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: now}); err != nil {
				return err
			}
			f.leases = append(f.leases, tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: item.NativeID, MachineID: tracker.MachineID(machineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: policy.ID, ExpiresAt: lease.ExpiresAt})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, lease := range f.leases {
		data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
		f.data = append(f.data, data)
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start_" + data.AttemptID}, Type: "run.started", SchemaVersion: 1, Data: data}
		path := "/api/v2/organizations/" + string(organization) + "/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, f.tokens[index], event), http.StatusOK)
	}
	return f
}

func (f runnerPGFixture) client(t *testing.T, index int) (*api.NativeClient, context.Context) {
	t.Helper()
	client, err := api.New(api.Config{URL: f.server.URL, TokenSource: func() string { return f.tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.organization, "prj_runner")
	if err != nil {
		t.Fatal(err)
	}
	return native, api.WithMutationAuthority(t.Context(), native.Scope(), f.leases[index], nil)
}

func TestPostgresRunnerSessionMutationOrdersLocksWithLeaseRenewal(t *testing.T) {
	for _, name := range []string{"session", "progress", "progress with another runner"} {
		t.Run(name, func(t *testing.T) {
			f := newRunnerPGFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mutate func() error
			if name == "session" {
				native, sessionContext := f.client(t, 0)
				sessions := api.NewRunnerSessions(native)
				id, err := sessions.StartSession(sessionContext, store.SessionStart{IssueID: string(f.leases[0].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"})
				if err != nil {
					t.Fatal(err)
				}
				mutate = func() error {
					return sessions.UpdateSessionWorkerProcess(sessionContext, id, store.WorkerProcessRegistration{CleanupRoot: "/example/workspace", CleanupPath: "/example/workspace/scratch"})
				}
			} else {
				index := 0
				if name == "progress with another runner" {
					index = 1
				}
				credential, status, err := f.service.authenticateAccessHash(ctx, apikey.HashToken(f.tokens[index]), "", "")
				if err != nil || status != http.StatusOK {
					t.Fatalf("authenticate: status=%d err=%v", status, err)
				}
				owner := nativeProgressOwner{service: f.service, scope: nativeScope{organization: "org_startup", project: "prj_runner", credential: credential}, request: progresscheck.Request{IssueID: string(f.leases[0].WorkItemID), PolicyID: f.policy.PolicyID, Instance: string(f.bindings[0].MachineID)}, leaseID: f.leases[0].ID, fencingToken: f.leases[0].FencingToken}
				mutate = func() error {
					return f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error { return owner.validateProgressLeaseWriter(ctx, w) })
				}
			}
			check := func(err error) {
				t.Helper()
				if name == "progress with another runner" {
					var refusal *hubapi.NativeError
					if !errors.As(err, &refusal) || refusal.Code != "unauthorized" {
						t.Fatalf("mismatched runner error=%v", err)
					}
				} else if err != nil {
					t.Fatalf("mutation refused after renewal: %v", err)
				}
			}
			renewal, err := f.admin.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = renewal.Rollback() })
			var runnerID string
			if err := renewal.QueryRowContext(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id='org_startup' AND runner_id=$1 FOR UPDATE`, f.bindings[0].RunnerID).Scan(&runnerID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() { defer close(done); result <- mutate() }()
			t.Cleanup(func() { cancel(); _ = renewal.Rollback(); <-done })
			for {
				var waiting bool
				if err := f.admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%runner_identities%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("mutation finished while runner identity locked: %v", err)
				default:
				}
			}
			if _, err := renewal.ExecContext(ctx, `SELECT lease_id FROM leases WHERE organization_id='org_startup' AND lease_id=$1 FOR UPDATE`, string(f.leases[0].ID)); err != nil {
				t.Fatalf("renewal lock order deadlocked with mutation: %v", err)
			}
			if err := renewal.Commit(); err != nil {
				t.Fatal(err)
			}
			check(<-result)
			check(mutate())
		})
	}
}

type runnerSessionClockReader interface{ runnersession.Reader }

type runnerSessionFaultReader struct {
	tenantstore.Reader
	runnerSessionClockReader
	attemptError error
	policyError  error
}

func (r runnerSessionFaultReader) RunnerSessionTime(ctx context.Context) (time.Time, error) {
	return r.Reader.(runnersession.Reader).RunnerSessionTime(ctx)
}

func (r runnerSessionFaultReader) LatestAttempt(ctx context.Context, id int64) (leasestore.Attempt, error) {
	if r.attemptError != nil {
		return leasestore.Attempt{}, r.attemptError
	}
	return r.Reader.LatestAttempt(ctx, id)
}

func (r runnerSessionFaultReader) ProjectPolicyRevision(ctx context.Context, id tracker.ProjectID, policyID string) (policystore.Revision, error) {
	if r.policyError != nil {
		return policystore.Revision{}, r.policyError
	}
	return r.Reader.ProjectPolicyRevision(ctx, id, policyID)
}

func TestPostgresRunnerSessionFencePreservesStorageErrors(t *testing.T) {
	f := newRunnerPGFixture(t)
	credential, status, err := f.service.authenticateAccessHash(t.Context(), apikey.HashToken(f.tokens[0]), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("authenticate: status=%d err=%v", status, err)
	}
	scope := nativeScope{organization: "org_startup", project: "prj_runner", credential: credential}
	request := runnersession.Mutation{LeaseID: f.leases[0].ID, FencingToken: f.leases[0].FencingToken}
	for _, test := range []struct {
		name                  string
		attempt, policy, want error
	}{
		{"attempt storage", corestore.ErrSerializationFailure, nil, corestore.ErrSerializationFailure},
		{"policy storage", nil, corestore.ErrSerializationFailure, corestore.ErrSerializationFailure},
		{"attempt missing", corestore.ErrNotFound, nil, tracker.ErrStaleFencingToken},
		{"policy missing", nil, corestore.ErrNotFound, tracker.ErrStaleFencingToken},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				_, _, err := f.service.runnerSessionFence(t.Context(), runnerSessionFaultReader{Reader: r, runnerSessionClockReader: r.(runnersession.Reader), attemptError: test.attempt, policyError: test.policy}, scope, request, string(f.leases[0].WorkItemID), false)
				if !errors.Is(err, test.want) {
					t.Errorf("fence error=%v want=%v", err, test.want)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
