package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type recoveryClaimInterleaveKey struct{}
type recoveryClaimInterleave struct {
	*postgres.Repository
	reached, release chan struct{}
	abort            atomic.Bool
}
type recoveryClaimInterleaveWriter struct {
	tenantstore.Writer
	interleave *recoveryClaimInterleave
}

func (r *recoveryClaimInterleave) WriteRunnerCommand(ctx context.Context, org tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	return r.Repository.WriteRunnerCommand(ctx, org, credential, runner, func(ctx context.Context, w tenantstore.Writer) error {
		if ctx.Value(recoveryClaimInterleaveKey{}) == true {
			w = recoveryClaimInterleaveWriter{w, r}
		}
		return write(ctx, w)
	})
}
func (w recoveryClaimInterleaveWriter) LockRelationshipGraph(ctx context.Context) error {
	close(w.interleave.reached)
	select {
	case <-w.interleave.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if w.interleave.abort.Load() {
		return errors.New("release unsafe lock interleave")
	}
	return w.Writer.LockRelationshipGraph(ctx)
}

func TestRecordedRecoveryDoesNotHoldGraphWhileWaitingForClaimRunner(t *testing.T) {
	f := newRunnerPGFixture(t)
	cfg, err := f.service.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 4
	pgtransport.Configure(cfg)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	originalPostgres, originalAccess, originalTenant := f.service.database.postgres, f.service.config.AccessRepository, f.service.config.TenantRepository
	t.Cleanup(func() {
		f.service.database.postgres, f.service.config.AccessRepository, f.service.config.TenantRepository = originalPostgres, originalAccess, originalTenant
	})
	f.service.database.postgres = repository
	f.service.config.AccessRepository = repository
	f.service.config.TenantRepository = repository
	org := tracker.OrganizationID("org_startup")
	project := tracker.ProjectID("prj_runner")
	base := "/api/v2/organizations/org_startup/projects/prj_runner"
	path := base + "/work-items/" + string(f.leases[0].WorkItemID)
	data := f.data[0]
	data.Sequence = 2
	data.Outcome = "succeeded"
	data.Disposition = &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, ReasonCode: "instance_limitation", BlockerEvidence: []workpad.Blocker{{Ref: "instance:example", Reason: "Example repair", Owner: workpad.BlockerOwnerInstance}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.tokens[0], tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "finish-blocked"}, Type: "run.finished", SchemaVersion: 1, Data: data}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/leases/"+string(f.leases[0].ID)+"/release", f.tokens[0], tracker.NativeLeaseMutation{FencingToken: f.leases[0].FencingToken, Reason: "completed"}), http.StatusNoContent)
	var issue tracker.NativeIssue
	currentResponse := performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[0], nil)
	requireNativeStatus(t, currentResponse, http.StatusOK)
	decodeHubResponse(t, currentResponse, &issue)
	issue.State = "Blocked"
	issue.Profile = "native"
	issue.Body = workItemRecordBody
	issue.CreatedAt = f.policy.ApprovedAt
	issue.Revision++
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	historyRaw, err := json.Marshal(tracker.CollaborationEvent{ID: "event-historical-blocked", OrganizationID: org, ProjectID: project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "credential_1"}, RecordedAt: time.Now().UTC(), Data: tracker.CollaborationData{Revision: issue.Revision, FromState: "Todo", ToState: "Blocked", Reason: "worker_progress"}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), org, historyRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		p, err := w.ProjectRecord(t.Context(), project)
		if err != nil {
			return err
		}
		p.States = append(p.States, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
		if _, err = w.UpdateProjectRecord(t.Context(), p); err != nil {
			return err
		}
		row, err := w.WorkItem(t.Context(), project, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.WorkflowStateName = &issue.State
		if _, err = w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if _, err = w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		if _, err = w.AppendEvent(t.Context(), event); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), f.bindings[0].RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "recovery_admin", Name: "Example recovery administrator", Hash: apikey.HashToken("example_recovery_admin"), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "personal", UserID: "user_example", MembershipID: "member_example", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "user_example", Email: "example@example.test", MembershipID: "member_example", Role: "owner", Active: true, PrincipalID: "recovery_admin", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	provider := f.service.config.Hosted.Provider.(*hostedSecurityProvider)
	provider.mu.Lock()
	member := auth.Membership{ID: "member_example", UserID: "user_example", OrganizationID: "provider_org_startup", Status: "active"}
	member.Role.Slug = "owner"
	provider.members[member.ID] = member
	provider.mu.Unlock()
	selectPostgresHubTestOrganization(f.service, org)
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/work-items", "example_recovery_admin", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "competing-claim"}, Title: "Example competing issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var other tracker.NativeIssue
	decodeHubResponse(t, response, &other)

	currentResponse = performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[0], nil)
	requireNativeStatus(t, currentResponse, http.StatusOK)
	decodeHubResponse(t, currentResponse, &issue)
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(f.tokens[0]), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("recovery fixture authentication: %d %v", status, err)
	}
	scope := nativeScope{organization: org, project: project, credential: credential}
	var source corestore.WorkItem
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		source, err = r.WorkItem(t.Context(), project, issue.WorkItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recovery := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "recover-blocked", LeaseID: f.leases[0].ID, FencingToken: f.leases[0].FencingToken}, ExpectedRevision: issue.Revision, State: "Todo", Reason: "dependency_ready", ReasonDetail: "recorded blocker recovery", BlockerAttemptID: data.AttemptID, PolicyID: f.policy.PolicyID}
	if _, err := f.service.prepareRecordedRecoveryTenant(t.Context(), scope, source, issue, recovery); err != nil {
		t.Fatalf("invalid recovery fixture: %v", err)
	}
	interleave := &recoveryClaimInterleave{Repository: repository, reached: make(chan struct{}), release: make(chan struct{})}
	f.service.config.TenantRepository = interleave
	f.admin.SetMaxOpenConns(2)
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	var workers sync.WaitGroup
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(interleave.release) }); cancel(); workers.Wait() })
	claim := tracker.NativeClaim{PolicyID: f.policy.PolicyID, WorkItemID: other.WorkItemID, MachineID: f.bindings[0].MachineID, SessionID: "competing-claim", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeWorkspaceCapability}}
	claimRequest := newHubAPIRequest(t, http.MethodPost, base+"/claims", f.tokens[0], claim).WithContext(context.WithValue(ctx, recoveryClaimInterleaveKey{}, true))
	claimResponse := httptest.NewRecorder()
	claimDone := make(chan struct{})
	workers.Go(func() { defer close(claimDone); f.service.Handler().ServeHTTP(claimResponse, claimRequest) })
	select {
	case <-interleave.reached:
	case <-claimDone:
		t.Fatalf("claim ended before graph: %d %s", claimResponse.Code, claimResponse.Body)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var claimPID int
	if err := f.admin.QueryRowContext(ctx, `SELECT a.pid FROM pg_stat_activity a WHERE a.datname=current_database() AND a.state='idle in transaction' AND EXISTS(SELECT 1 FROM pg_locks l WHERE l.pid=a.pid AND l.relation='runner_identities'::regclass AND l.mode='RowShareLock' AND l.granted)`).Scan(&claimPID); err != nil {
		t.Fatal(err)
	}

	recoveryRequest := newHubAPIRequest(t, http.MethodPost, path+"/workflow", f.tokens[0], recovery).WithContext(ctx)
	recoveryResponse := httptest.NewRecorder()
	recoveryDone := make(chan struct{})
	workers.Go(func() { defer close(recoveryDone); f.service.Handler().ServeHTTP(recoveryResponse, recoveryRequest) })
	var waitingQuery string
	testenv.WaitUntil(t, ctx, func() error {
		select {
		case <-recoveryDone:
			t.Fatalf("recovery ended before runner wait: %d %s", recoveryResponse.Code, recoveryResponse.Body)
		default:
		}
		return f.admin.QueryRowContext(ctx, `SELECT query FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid))`, claimPID).Scan(&waitingQuery)
	})
	if !strings.Contains(waitingQuery, "runner_identities") || !strings.Contains(waitingQuery, "FOR UPDATE") {
		t.Fatalf("unexpected recovery lock wait: %s", waitingQuery)
	}
	t.Logf("claim pid=%d owns runner; recovery waits: %s", claimPID, waitingQuery)
	probe, err := f.admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, probeErr := probe.ExecContext(ctx, `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR NO KEY UPDATE NOWAIT`, org)
	if err := probe.Rollback(); err != nil {
		t.Fatal(err)
	}
	if probeErr != nil {
		interleave.abort.Store(true)
		t.Errorf("recovery holds graph while waiting for same-runner claim (SQLSTATE=%s): %v", pgtransport.SQLState(probeErr), probeErr)
	} else {
		t.Log("graph NOWAIT probe succeeds while recovery waits for runner")
	}
	release.Do(func() { close(interleave.release) })
	for _, done := range []chan struct{}{claimDone, recoveryDone} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if probeErr != nil {
		return
	}
	requireNativeStatus(t, claimResponse, http.StatusOK)
	requireNativeStatus(t, recoveryResponse, http.StatusOK)
	replay := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.tokens[0], recovery)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != recoveryResponse.Body.String() {
		t.Fatal("recovery replay changed committed receipt")
	}
	var recovered tracker.NativeIssue
	decodeHubResponse(t, recoveryResponse, &recovered)
	if recovered.State != "Todo" || recovered.Revision != issue.Revision+1 {
		t.Fatalf("recovery source mismatch: state=%s revision=%d", recovered.State, recovered.Revision)
	}
}
