package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type runnerPGFixture struct {
	admin    *sql.DB
	bindings []runnerauth.Binding
	service  *Service
	server   *httptest.Server
	policy   policystore.Revision
	leases   []tracker.NativeLease
	tokens   []string
	data     []tracker.NativeRunData
}

func TestPostgresRunnerAuditDispositionUsesArchivedSource(t *testing.T) {
	f := newRunnerPGFixture(t)
	for index, lease := range f.leases {
		native, ctx := f.client(t, index)
		now := time.Now().UTC().Truncate(time.Microsecond)
		sessionID, err := api.NewRunnerSessions(native).StartSession(ctx, store.SessionStart{IssueID: string(lease.WorkItemID), StartedAt: now, AgentRole: "security_audit", Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}
		key := securityaudit.Key{ProjectID: "prj_runner", Repository: "example-org/example-project", PRNumber: index + 1, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat(strconv.Itoa(index+1), 40)}
		changeID, versionID := newNativeID("change"), newNativeID("version")
		change := tracker.ChangeRequest{ID: changeID, OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now}
		changeRaw, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		changeRow, err := content.PrepareChange(ctx, "org_startup", changeRaw)
		if err != nil {
			t.Fatal(err)
		}
		version := tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, CreatedAt: now, ChangeVersionInput: tracker.ChangeVersionInput{Repository: key.Repository, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA, AttemptID: f.data[index].AttemptID, PolicyID: f.policy.PolicyID}}
		versionRaw, err := json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
		versionRow, err := content.PrepareVersion(ctx, "org_startup", versionRaw)
		if err != nil {
			t.Fatal(err)
		}
		run := securityaudit.Run{ID: sessionID, ProjectID: "prj_runner", Repository: key.Repository, PRNumber: key.PRNumber, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), Findings: []securityaudit.Finding{{ID: "finding_example", Body: "Example finding", Certainty: securityaudit.CertaintyConfirmed}}}
		review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: versionID, Decision: "changes_requested", CreatedAt: now, SecurityAudit: &tracker.SecurityAuditReview{SessionID: sessionID, VersionID: versionID, Run: run}}
		reviewRaw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		reviewRow, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: changeID, VersionID: &versionID, Kind: "review", CreatedAt: now}, reviewRaw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateChange(ctx, changeRow); err != nil {
				return err
			}
			if _, err := w.CreateVersion(ctx, versionRow); err != nil {
				return err
			}
			_, err := w.AppendEvidence(ctx, reviewRow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		disposition, err := native.RecordAuditDisposition(ctx, securityaudit.DispositionCommand{Key: key, FindingID: "finding_example", Evidence: "The exact source requires authenticated access.", Confirm: true})
		if err != nil || disposition.ID < 1 || disposition.AuditRunID != sessionID {
			t.Fatalf("Hub disposition: %+v %v", disposition, err)
		}
		restarted, restartedCtx := f.client(t, index)
		evidence, err := restarted.AuditEvidence(restartedCtx, key)
		if err != nil || evidence.Run.ID != sessionID || len(evidence.Dispositions) != 1 || evidence.Dispositions[0].Evidence != disposition.Evidence {
			t.Fatalf("restart lost audit disposition: %+v %v", evidence, err)
		}
		key.HeadSHA = strings.Repeat("f", 40)
		if _, err := native.RecordAuditDisposition(ctx, securityaudit.DispositionCommand{Key: key, FindingID: "finding_example", Evidence: "Wrong source", Confirm: true}); err == nil {
			t.Fatal("unrecorded audit source accepted a disposition")
		}
	}
}

type postgresUpdateReceipts struct {
	ctx     context.Context
	client  *api.Client
	running runnerauth.BuildEvidence
}

func (s postgresUpdateReceipts) Load() (*runnerauth.UpdateReceipt, error) {
	return s.client.RunnerUpdateReceipt(s.ctx)
}
func (s postgresUpdateReceipts) Save(receipt *runnerauth.UpdateReceipt) error {
	return s.client.StoreRunnerUpdateReceipt(s.ctx, receipt, s.running)
}

func TestPostgresRunnerUpdateReceiptRestoresWithoutLocalState(t *testing.T) {
	f := newRunnerPGFixture(t)
	for index, binding := range f.bindings {
		now := time.Now().UTC().Truncate(time.Microsecond)
		running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: runtime.GOOS, Architecture: runtime.GOARCH, VerifiedRelease: true, ObservedAt: now}
		request := runnerauth.UpdateRequest{ID: fmt.Sprintf("update_%d", index), Service: "detent", Version: "1.1.0", Release: true, ExpectedBuildRevision: strings.Repeat("c", 64), RequestedAt: now}
		if index == 1 {
			request.Urgent = true
			request.ExpectedBuildRevision = ""
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			requestRunner := binding.RunnerID
			if request.Urgent {
				requestRunner = ""
			}
			if err := w.CreateRunnerUpdateRequest(t.Context(), runnerstore.UpdateRequestRecord{OrganizationID: "org_startup", RunnerID: requestRunner, Request: request}); err != nil {
				return err
			}
			if request.Urgent {
				return w.SetRunnerUrgentUpdate(t.Context(), 0, request.ID, now)
			}
			return w.SetRunnerUpdateDesired(t.Context(), binding.RunnerID, 1, request.ID)
		}); err != nil {
			t.Fatal(err)
		}
		identityDirectory := t.TempDir()
		if err := os.Chmod(identityDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		identityPath := filepath.Join(identityDirectory, "identity.json")
		file := runnerauth.File{Schema: 1, HubURL: f.server.URL, Credential: f.tokens[index], Identity: runnerauth.Identity{Binding: binding, OrganizationID: "org_startup", Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, ExpiresAt: now.Add(time.Hour)}}
		raw, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(identityPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		newClient := func() *api.Client {
			client, err := api.New(api.Config{URL: f.server.URL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: &http.Transport{}}})
			if err != nil {
				t.Fatal(err)
			}
			return client
		}
		client := newClient()
		receipt := &runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}
		if err := client.StoreRunnerUpdateReceipt(t.Context(), receipt, running); err != nil {
			t.Fatal(err)
		}
		restarted := newClient()
		if _, err := detentupdate.NewScheduler(detentupdate.SchedulerConfig{CheckInterval: time.Minute, RunningBuild: running, ReceiptStore: postgresUpdateReceipts{ctx: t.Context(), client: restarted, running: running}}); err != nil {
			t.Fatal(err)
		}
		loaded, err := restarted.RunnerUpdateReceipt(t.Context())
		if err != nil || loaded == nil || loaded.Status != "uncertain" || loaded.Request.ID != request.ID || loaded.FailureReason == "" {
			t.Fatalf("restart lost update recovery receipt: %+v %v", loaded, err)
		}
		path := "/api/v2/organizations/org_startup/runners/" + binding.RunnerID + "/update/receipt"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.tokens[1-index], nil), http.StatusUnauthorized)
	}
}

func TestPostgresRunnerUsageAndCheckpointSurviveClientRestart(t *testing.T) {
	f := newRunnerPGFixture(t)
	day := time.Now().UTC()
	for index, lease := range f.leases {
		native, ctx := f.client(t, index)
		data := f.data[index]
		data.Sequence = 2
		data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
		data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: int64(17 + index), CachedInput: 2, Output: 3, BillingMode: "metered", Currency: "USD", To: day}}
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "usage_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}
		if err := native.AppendEvent(ctx, lease.WorkItemID, event); err != nil {
			t.Fatal(err)
		}
		if err := native.AppendEvent(ctx, lease.WorkItemID, event); err != nil {
			t.Fatalf("usage replay: %v", err)
		}
		data.Sequence = 3
		data.Handoff = &tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "git_ref", WorktreeState: "clean", HeadSHA: strings.Repeat("a", 40), Ref: tracker.CheckpointRefPrefix + string(lease.WorkItemID), CommitSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40), ExternalEffect: "none", EffectState: "none"}
		if err := native.AppendEvent(ctx, lease.WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "checkpoint_" + data.AttemptID}, Type: "run.checkpointed", SchemaVersion: 1, Data: data}); err != nil {
			t.Fatal(err)
		}
		restarted, restartedCtx := f.client(t, index)
		spend, err := restarted.IssueTokenSpend(restartedCtx, store.IssueIdentity{IssueID: string(lease.WorkItemID)})
		if err != nil || spend.InputTokens != int64(17+index) || spend.OutputTokens != 3 || spend.Sessions != 1 {
			t.Fatalf("usage replay changed spend: %+v %v", spend, err)
		}
		var attempt tracker.NativeAttempt
		if err := restarted.Transport().Request(restartedCtx, http.MethodGet, restarted.Scope()+"/work-items/"+string(lease.WorkItemID)+"/attempts/"+data.AttemptID, nil, &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.Checkpoint == nil || !attempt.Checkpoint.GitRef() || attempt.Checkpoint.CommitSHA != data.Handoff.CommitSHA || len(attempt.Usage) != 1 || attempt.Usage[0].Input != int64(17+index) {
			t.Fatalf("restart lost fenced checkpoint/usage: %+v", attempt)
		}
	}
	native, ctx := f.client(t, 0)
	spend, err := native.ProjectDailyTokenSpend(ctx, "local-project", day)
	if err != nil || spend.InputTokens != 35 || spend.OutputTokens != 6 || spend.Sessions != 2 {
		t.Fatalf("daily typed spend: %+v %v", spend, err)
	}
	quantiles, err := native.RecentModelTokenQuantiles(ctx, store.ModelTokenQuantileQuery{Model: "example-model", Limit: 50})
	if err != nil || quantiles.Sessions != 2 || quantiles.P90InputTokens != 18 || quantiles.P90OutputTokens != 3 {
		t.Fatalf("typed quantiles: %+v %v", quantiles, err)
	}
	data := f.data[0]
	data.Sequence = 4
	data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
	data.Usage = []tracker.NativeUsage{{Provider: "example", Model: "example-model", Input: 27, CachedInput: 2, Output: 3, BillingMode: "metered", Currency: "USD", To: day.AddDate(0, 0, 1)}}
	if err := native.AppendEvent(ctx, f.leases[0].WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "next_day_" + data.AttemptID}, Type: "run.observed", SchemaVersion: 1, Data: data}); err != nil {
		t.Fatal(err)
	}
	previous, err := native.ProjectDailyTokenSpend(ctx, "local-project", day)
	if err != nil || previous.InputTokens != 35 || previous.OutputTokens != 6 {
		t.Fatalf("new day moved previously accounted spend: %+v %v", previous, err)
	}
	next, err := native.ProjectDailyTokenSpend(ctx, "local-project", day.AddDate(0, 0, 1))
	if err != nil || next.InputTokens != 10 || next.OutputTokens != 0 {
		t.Fatalf("new day did not account only incremental spend: %+v %v", next, err)
	}

}

func TestPostgresRunnerRestartDoesNotReleaseLiveProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX process groups")
	}
	f := newRunnerPGFixture(t)
	native, ctx := f.client(t, 0)
	sessions := api.NewRunnerSessions(native)
	id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(f.leases[0].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"})
	if err != nil {
		t.Fatal(err)
	}
	restarted, restartedCtx := f.client(t, 0)
	stopped, err := restarted.ReleasedProvidersStopped(restartedCtx, f.data[0].AttemptID)
	if err != nil || stopped {
		t.Fatalf("unregistered provider state authorized cleanup: %t %v", stopped, err)
	}
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	procgroup.Configure(t.Context(), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	identity, err := procgroup.Inspect(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.UpdateSessionWorkerProcess(ctx, id, store.WorkerProcessRegistration{WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt}}); err != nil {
		t.Fatal(err)
	}
	stopped, err = restarted.ReleasedProvidersStopped(restartedCtx, f.data[0].AttemptID)
	if err != nil || stopped {
		t.Fatalf("live Hub-recorded provider authorized cleanup: %t %v", stopped, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	stopped, err = restarted.ReleasedProvidersStopped(restartedCtx, f.data[0].AttemptID)
	if err != nil || !stopped {
		t.Fatalf("stopped Hub-recorded provider could not reconcile: %t %v", stopped, err)
	}
}

func TestPostgresRunnerProcessRegistrationRequiresSessionAuthority(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses POSIX process groups")
	}
	f := newRunnerPGFixture(t)
	native, ctx := f.client(t, 0)
	trusted := api.NewRunnerSessions(native)
	attackerNative, attackerCtx := f.client(t, 0)
	attacker := api.NewRunnerSessions(attackerNative)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, victim := filepath.Join(base, "workspace"), filepath.Join(base, "victim")
	scratch, secret := filepath.Join(workspaceRoot, "scratch"), filepath.Join(victim, "secret")
	for _, dir := range []string{scratch, victim} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	identities := make([]store.WorkerProcessIdentity, 2)
	for index := range identities {
		cmd := exec.CommandContext(t.Context(), "sleep", "30")
		procgroup.Configure(t.Context(), cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		identity, err := procgroup.Inspect(cmd)
		if err != nil {
			t.Fatal(err)
		}
		identities[index] = store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt}
	}
	owned, unrelated := identities[0], identities[1]
	start := store.SessionStart{IssueID: string(f.leases[0].WorkItemID), StartedAt: time.Now().UTC(), Model: "example-model"}
	trustedID, err := trusted.StartSession(ctx, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := trusted.UpdateSessionWorkerProcess(ctx, trustedID, store.WorkerProcessRegistration{CleanupRoot: workspaceRoot, CleanupPath: scratch}); err != nil {
		t.Fatalf("trusted reservation refused: %v", err)
	}
	if err := trusted.UpdateSessionWorkerProcess(ctx, trustedID, store.WorkerProcessRegistration{WorkerProcessIdentity: owned, CleanupRoot: workspaceRoot, CleanupPath: scratch}); err != nil {
		t.Fatalf("trusted started registration refused: %v", err)
	}
	forged := store.WorkerProcessRegistration{WorkerProcessIdentity: unrelated, CleanupRoot: victim, CleanupPath: secret}
	for name, err := range map[string]error{
		"reserved overwrite": attacker.UpdateSessionWorkerProcess(attackerCtx, trustedID, store.WorkerProcessRegistration{CleanupRoot: victim, CleanupPath: secret}),
		"started overwrite":  attacker.UpdateSessionWorkerProcess(attackerCtx, trustedID, forged),
		"reap":               attacker.MarkSessionWorkerProcessReaped(attackerCtx, trustedID, store.WorkerProcessReap{ReapedAt: time.Now().UTC(), Outcome: "exited", Reason: "forged"}),
	} {
		if err == nil {
			t.Fatalf("Hub accepted %s without the session process authority", name)
		}
	}
	foreignID, err := attacker.StartSession(attackerCtx, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := attacker.UpdateSessionWorkerProcess(attackerCtx, foreignID, forged); err != nil {
		t.Fatal(err)
	}
	restartedNative, restartedCtx := f.client(t, 0)
	restarted := api.NewRunnerSessions(restartedNative)
	for name, sessions := range map[string]*api.RunnerSessions{"trusted": trusted, "restarted": restarted} {
		listCtx := ctx
		if name == "restarted" {
			listCtx = restartedCtx
		}
		workers, err := sessions.ListActiveWorkerProcesses(listCtx)
		if err != nil {
			t.Fatal(err)
		}
		var terminated []store.WorkerProcessIdentity
		for _, worker := range workers {
			terminated = append(terminated, worker.WorkerProcessIdentity)
			if err := workspace.CleanupOwnedPath(worker.CleanupRoot, worker.CleanupPath); err != nil {
				t.Fatal(err)
			}
		}
		if slices.Contains(terminated, unrelated) {
			t.Fatalf("%s client would terminate a forged process identity: %+v", name, workers)
		}
		if name == "trusted" && (len(workers) != 1 || workers[0].SessionID != trustedID || workers[0].WorkerProcessIdentity != owned || workers[0].CleanupRoot != workspaceRoot || workers[0].CleanupPath != scratch) {
			t.Fatalf("trusted client lost its own registration: %+v", workers)
		}
		if name == "restarted" && len(workers) != 0 {
			t.Fatalf("restarted client trusted Hub-only registrations: %+v", workers)
		}
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("trusted cleanup did not remove its scratch: %v", err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("Hub-supplied cleanup path deleted a file outside the local registration: %v", err)
	}
	if err := trusted.MarkSessionWorkerProcessReaped(ctx, trustedID, store.WorkerProcessReap{ReapedAt: time.Now().UTC(), Outcome: "exited", Reason: "test"}); err != nil {
		t.Fatalf("trusted reap refused: %v", err)
	}
}

func newRunnerPGFixture(t *testing.T, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	seedStartupOrganization(t, service.config.TenantRepository, "org_startup")
	f := runnerPGFixture{admin: admin, service: service, server: httptest.NewServer(service.Handler())}
	t.Cleanup(f.server.Close)
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	descriptor := hubTestPolicy()
	for _, edit := range customize {
		edit(&descriptor)
	}
	policy := descriptor.WithID()
	f.policy, err = eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), "org_startup", policy)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.ProjectID = "prj_runner"
	f.policy.ApprovedAt, f.policy.ApprovedBy = now, "test_hub_admin"
	err = service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: "org_startup", ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_startup", ID: "prj_runner", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
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
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: credentialID, Name: "Example runner " + id, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: credentialID, ProjectID: "prj_runner"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_startup", ID: "enrollment_" + id, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("example_enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_"+id, runnerID, tracker.MachineID(machineID), now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_startup", ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_startup", RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: credentialID, EnrollmentID: "enrollment_" + id, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{"prj_runner"}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			state := "Todo"
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_startup", ProjectID: "prj_runner", NativeID: tracker.NativeWorkItemID("wi_" + id), Number: int64(index), Title: "Example", WorkflowStateName: &state})
			if err != nil {
				return err
			}
			itemRaw, err := json.Marshal(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: "Example", State: "Todo"})
			if err != nil {
				return err
			}
			itemVersion, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: item.Revision}, itemRaw)
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
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: "org_startup", LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: "org_startup", ProjectID: "prj_runner", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: now}); err != nil {
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
		data := tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
		f.data = append(f.data, data)
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start_" + data.AttemptID}, Type: "run.started", SchemaVersion: 1, Data: data}
		path := "/api/v2/organizations/org_startup/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
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
	native, err := client.Native("org_startup", "prj_runner")
	if err != nil {
		t.Fatal(err)
	}
	return native, api.WithMutationAuthority(t.Context(), native.Scope(), f.leases[index], nil)
}

func TestPostgresRunnerSessionRestartRetainsFencedEvidence(t *testing.T) {
	f := newRunnerPGFixture(t)
	for index := range f.leases {
		native, ctx := f.client(t, index)
		sessions := api.NewRunnerSessions(native)
		started := time.Now().UTC().Truncate(time.Microsecond)
		start := store.SessionStart{ProjectID: "local-project", IssueID: string(f.leases[index].WorkItemID), StartedAt: started, RequestedModel: "example-model", Model: "example-model", AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code"}
		committed := make(chan runnersession.Snapshot, 1)
		lostResponse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, r)
			var snapshot runnersession.Snapshot
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.ID < 1 {
				t.Error("session start did not commit before response loss")
			}
			committed <- snapshot
			w.Header().Set("Content-Length", strconv.Itoa(response.Body.Len()+1))
			w.WriteHeader(response.Code)
			if _, err := w.Write(response.Body.Bytes()); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(lostResponse.Close)
		transport, err := api.New(api.Config{URL: lostResponse.URL, TokenSource: func() string { return f.tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
		if err != nil {
			t.Fatal(err)
		}
		uncertainNative, err := transport.Native("org_startup", "prj_runner")
		if err != nil {
			t.Fatal(err)
		}
		uncertainID, err := api.NewRunnerSessions(uncertainNative).StartSession(ctx, start)
		if uncertainID != 0 || !errors.Is(err, api.ErrUnavailable) {
			t.Fatalf("lost start response: id=%d error=%v", uncertainID, err)
		}
		orphan := <-committed
		lostResponse.Close()
		var persistedStart runnersession.Snapshot
		if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/runner-sessions/"+strconv.FormatInt(orphan.ID, 10), nil, &persistedStart); err != nil {
			t.Fatal(err)
		}
		if persistedStart.Revision != 1 || persistedStart.Start.IssueID != start.IssueID || persistedStart.Provider.ThreadID != "" || persistedStart.Provider.SessionID != "" || persistedStart.Worker != nil || persistedStart.Progress != nil || persistedStart.Finish != nil || orphan.ReceiptID < 1 {
			t.Fatalf("lost response changed committed pre-provider session: %+v", persistedStart)
		}
		if stopped, err := native.ReleasedProvidersStopped(ctx, f.data[index].AttemptID); err != nil || stopped {
			t.Fatalf("pre-provider session process observation: stopped=%t error=%v", stopped, err)
		}
		if _, err := sessions.LatestIssueAgentResumeState(ctx, store.IssueIdentity{IssueID: start.IssueID}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("pre-provider session selected for current-attempt resume: %v", err)
		}
		id, err := sessions.StartSession(ctx, start)
		if id == orphan.ID {
			t.Fatal("new start reused an uncertain request's session")
		}
		if err != nil || id < 1 {
			t.Fatalf("start: id=%d error=%v", id, err)
		}
		provider := store.SessionProviderIdentity{ThreadID: fmt.Sprintf("thread_%d", index), SessionID: fmt.Sprintf("provider_%d", index)}
		identity := agentidentity.Identity{BackendID: "example", BackendKind: "example", Role: "code", ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceConfigured)}
		if err := sessions.UpdateSessionIdentity(ctx, id, identity); err != nil {
			t.Fatal(err)
		}
		if err := sessions.UpdateSessionProviderIdentity(ctx, id, provider); err != nil {
			t.Fatal(err)
		}
		progress := store.SessionProgress{HeadSHA: strings.Repeat("a", 40), LastProgressAt: started}
		if err := sessions.SaveSessionProgress(ctx, id, progress); err != nil {
			t.Fatal(err)
		}
		if err := sessions.UpdateSessionResumeState(ctx, id, store.SessionResumeState{ResumedFromSessionID: id, OrphanRecoveryOutcome: "resumed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{SessionID: id, PhaseName: "agent_active", StartedAt: started, FinishedAt: started.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.SaveWorkflowActivityProfile(ctx, 0, store.WorkflowPhaseEvent{SessionID: id, PhaseName: "profile"}, workflowmetrics.ActivityProfile{Schema: 1, SessionID: id, Status: "running"}); err != nil {
			t.Fatal(err)
		}
		usageReceipt, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{SessionID: id, Model: "example-model", InputTokens: 17, OutputTokens: 3})
		if err != nil {
			t.Fatal(err)
		}
		restartedNative, restartedCtx := f.client(t, index)
		restarted := api.NewRunnerSessions(restartedNative)
		resume, err := restarted.LatestAgentResumeState(restartedCtx, store.AgentResumeLookup{NativeAttemptID: f.data[index].AttemptID, IssueID: string(f.leases[index].WorkItemID), AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code", RequestedModel: "example-model"})
		if err != nil || resume.DetentSessionID != id || resume.ProviderThreadID != provider.ThreadID || resume.ProviderSessionID != provider.SessionID || !resume.Orphaned || resume.RuntimeIdentity.ResolvedModel.Value != "example-model" {
			t.Fatalf("restart lost provider identity: %+v %v", resume, err)
		}
		replacement, replacementCtx := f.client(t, 1-index)
		recovered, err := api.NewRunnerSessions(replacement).LatestAgentResumeState(replacementCtx, store.AgentResumeLookup{NativeAttemptID: f.data[index].AttemptID, IssueID: string(f.leases[index].WorkItemID), AgentBackendID: "example", AgentBackendKind: "example", AgentRole: "code", RequestedModel: "example-model"})
		if err != nil || recovered.DetentSessionID != id || recovered.ProviderThreadID != provider.ThreadID {
			t.Fatalf("replacement runner lost Hub resume identity: %+v %v", recovered, err)
		}
		persisted, err := restarted.SessionProgress(restartedCtx, id)
		if err != nil || persisted.HeadSHA != progress.HeadSHA || !persisted.LastProgressAt.Equal(started) {
			t.Fatalf("restart lost progress: %+v %v", persisted, err)
		}
		pinned, err := restarted.SessionPolicy(restartedCtx, "local-project", id)
		if err != nil || pinned.ID != f.policy.PolicyID {
			t.Fatalf("restart lost policy: %+v %v", pinned, err)
		}
		if err := restarted.FinishSession(restartedCtx, id, store.SessionFinish{CompletedAt: started.Add(time.Second), FinalState: "completed", ProviderThreadID: provider.ThreadID, ProviderSessionID: provider.SessionID}); err != nil {
			t.Fatal(err)
		}
		var history []runnersession.History
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			history, err = r.(runnersession.Reader).RunnerSessionHistory(t.Context(), "prj_runner", id, 0, 20)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(history) != 9 {
			t.Fatalf("session history lost writes: %d", len(history))
		}
		if usageReceipt <= 0 || history[7].ID != usageReceipt {
			t.Fatalf("usage receipt does not identify durable history: receipt=%d history=%+v", usageReceipt, history[7])
		}
		var archived runnersession.Mutation
		if err := restartedNative.Transport().Request(restartedCtx, http.MethodGet, restartedNative.Scope()+"/runner-sessions/"+strconv.FormatInt(id, 10)+"/history/7", nil, &archived); err != nil {
			t.Fatal(err)
		}
		if archived.Profile == nil || archived.Profile.Profile.SessionID != id {
			t.Fatalf("activity profile was not retained in Hub history: %+v", archived)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(f.leases[index].ID), FencingToken: int64(f.leases[index].FencingToken)}, "released")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := restarted.SaveSessionProgress(restartedCtx, id, progress); err == nil {
			t.Fatal("revoked lease accepted a session mutation")
		}
		other, _ := f.client(t, 1-index)
		if err := api.NewRunnerSessions(other).UpdateSessionProviderIdentity(restartedCtx, id, provider); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
			t.Fatalf("other runner accepted identity mutation: %v", err)
		}
	}
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
					var refusal *nativeError
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

func (r runnerSessionFaultReader) ProjectPolicy(ctx context.Context, id tracker.ProjectID) (policystore.Revision, error) {
	if r.policyError != nil {
		return policystore.Revision{}, r.policyError
	}
	return r.Reader.ProjectPolicy(ctx, id)
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
