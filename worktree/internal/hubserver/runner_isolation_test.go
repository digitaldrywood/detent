package hubserver

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerIsolationClaims(t *testing.T) {
	for _, test := range []struct {
		name         string
		report       isolation.Report
		tier         string
		want         int
		requiredTags []string
	}{
		{"missing report", nil, "sandbox", http.StatusConflict, nil},
		{"sandbox", isolation.Report{"codex": {"sandbox", "native-trusted"}}, "sandbox", http.StatusOK, nil},
		{"native cannot sandbox", isolation.Report{"codex": {"native-trusted"}}, "sandbox", http.StatusConflict, nil},
		{"trusted", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusOK, nil},
		{"sandbox project selector excludes trusted runner", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusConflict, []string{"sandbox"}},
		{"mixed backends", isolation.Report{"codex": {"sandbox"}, "claude": {"native-trusted"}}, "sandbox", http.StatusConflict, nil},
		{"probe failed", isolation.Report{"codex": {}}, "sandbox", http.StatusConflict, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.redemption.BackendIsolation = test.report
			r.enroll(t)
			issue := f.create(t, "queued")
			settings := map[string]any{"expected_revision": 1, "display_name": "Runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}, "isolation_tier": test.tier}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, settings), http.StatusOK)
			descriptor := hubTestPolicy()
			descriptor.Requirements = policy.Requirements{RequiredTags: test.requiredTags}
			descriptor.ID = ""
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim), test.want)
		})
	}
}

func TestRunnerEnrollmentIsolationSelection(t *testing.T) {
	for _, test := range []struct {
		name, selected, want string
		status               int
	}{
		{"legacy", "", "sandbox", http.StatusCreated},
		{"sandbox", "sandbox", "sandbox", http.StatusCreated},
		{"full access", "native-trusted", "native-trusted", http.StatusCreated},
		{"invalid", "root", "", http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.redemption.IsolationTier = test.selected
			response := performHubAPIRequest(t, f.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption)
			requireNativeStatus(t, response, test.status)
			if test.want == "" {
				return
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var runner runnerauth.Runner
			decodeHubResponse(t, response, &runner)
			if runner.IsolationTier != test.want {
				t.Fatalf("tier=%q, want %q", runner.IsolationTier, test.want)
			}
		})
	}
}

func TestRunnerIsolationHeartbeatWithdrawsTier(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.redemption.BackendIsolation = isolation.Report{"codex": {"sandbox", "native-trusted"}}
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"codex": {"native-trusted"}}}), http.StatusOK)
	var report string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT backend_isolation_json FROM runner_identities WHERE id = ?", r.binding.RunnerID).Scan(&report); err != nil {
		t.Fatal(err)
	}
	if report != `{"codex":["native-trusted"]}` {
		t.Fatalf("report = %s", report)
	}
}

func TestIsolationMigrationPreservesRecordings(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE runner_identities (id TEXT PRIMARY KEY); CREATE TABLE lease_runners (lease_id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	legacy, err := migrationFiles.ReadFile("migrations/00033_workspace_terminal_recordings.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), strings.Split(string(legacy), "-- +goose Down")[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workspace_terminal_recordings
 (id, workspace_id, organization_id, project_id, relay_session_id, stream_id, principal_id, subject, isolation, started_at, terminal_cols, terminal_rows, cast_text, cast_bytes, created_at, updated_at)
 VALUES ('legacy', 'workspace', 'org', 'project', 'relay', 'stream', 'person', 'subject', 'container', 'start', 80, 24, 'recorded output', 15, 'created', 'updated')`); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/00045_runner_isolation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), strings.Split(string(migration), "-- +goose Down")[0]); err != nil {
		t.Fatal(err)
	}
	var tier, cast string
	if err := db.QueryRowContext(t.Context(), "SELECT isolation, cast_text FROM workspace_terminal_recordings WHERE id = 'legacy'").Scan(&tier, &cast); err != nil {
		t.Fatal(err)
	}
	if tier != "container" || cast != "recorded output" {
		t.Fatalf("legacy recording = %q %q", tier, cast)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE workspace_terminal_recordings SET isolation = 'sandbox' WHERE id = 'legacy'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE workspace_terminal_recordings SET isolation = 'invalid' WHERE id = 'legacy'"); err == nil {
		t.Fatal("invalid isolation accepted")
	}
}

func TestRunnerIsolationOmittedHeartbeatClearsReport(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"display_name": "Runner", "capacity": 2, "version": "test"}), http.StatusOK)
	var report string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT backend_isolation_json FROM runner_identities WHERE id = ?", r.binding.RunnerID).Scan(&report); err != nil {
		t.Fatal(err)
	}
	if report != "{}" {
		t.Fatalf("stale report retained: %s", report)
	}
}

func TestRunnerIsolationPolicyPinnedToClaim(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim)
	r.enroll(t)
	issue := f.create(t, "queued")
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "pinned", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.IsolationPolicy == nil || lease.IsolationPolicy.Tier != isolation.Sandbox {
		t.Fatalf("claim isolation = %#v", lease.IsolationPolicy)
	}
	change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}, IsolationTier: isolation.NativeTrusted}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
	for _, request := range []struct {
		path string
		body any
	}{
		{f.base + "/claims", claim},
		{f.base + "/leases/" + string(lease.ID) + "/renew", tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}},
	} {
		response := performHubAPIRequest(t, f.service, http.MethodPost, request.path, r.redemption.Credential, request.body)
		requireNativeStatus(t, response, http.StatusOK)
		var renewed tracker.NativeLease
		decodeHubResponse(t, response, &renewed)
		if renewed.IsolationPolicy == nil || renewed.IsolationPolicy.Tier != isolation.Sandbox {
			t.Fatalf("claim downgraded: %#v", renewed.IsolationPolicy)
		}
	}
}
