package runnerfleet_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerupdate"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerIsolationHeartbeatWithdrawsTier(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		previousSandbox bool
		sandbox         bool
	}{
		{"previously proven sandbox", true, true},
		{"first sandbox probe", false, true},
		{"failed sandbox probe", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.Redemption.IsolationTier = isolation.NativeTrusted
			r.Redemption.SpriteName = r.Redemption.Hostname
			r.Redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
			if test.previousSandbox {
				r.Redemption.BackendIsolation["codex"] = append(r.Redemption.BackendIsolation["codex"], isolation.Sandbox)
			}
			r.Enroll(t)
			heartbeat := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": isolation.Report{"codex": {isolation.NativeTrusted}},
				"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: f.Hub.Now()}}
			postHeartbeat := func() {
				t.Helper()
				hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, heartbeat), http.StatusOK)
			}
			postHeartbeat()
			var before runnerauth.Runner
			hubfixture.Decode(t, f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/routing", r.Redemption.Credential, nil), &before)
			if isolation.Report(before.BackendIsolation).Supports(isolation.Sandbox) || !isolation.Report(before.BackendIsolation).Supports(isolation.NativeTrusted) {
				t.Fatalf("native heartbeat retained sandbox proof: %+v", before.BackendIsolation)
			}
			request := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: before.Routing}, IsolationTier: new(isolation.Sandbox)}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, request), http.StatusOK)
			issue := f.Create(t, "queued")
			descriptor := hubfixture.Policy()
			hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", descriptor)
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "sandbox-transition", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim), http.StatusConflict)
			want := http.StatusConflict
			if test.sandbox {
				heartbeat["backend_isolation"] = isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}}
				want = http.StatusOK
			} else {
				problem := runnerauth.NewProblem("tier_unavailable")
				problem.Subject = "codex/sandbox"
				heartbeat["problems"] = []runnerauth.Problem{problem}
			}
			postHeartbeat()
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim)
			hubfixture.RequireStatus(t, response, want)
			if test.sandbox {
				var lease tracker.NativeLease
				hubfixture.Decode(t, response, &lease)
				if lease.IsolationPolicy == nil || lease.IsolationPolicy.Tier != isolation.Sandbox {
					t.Fatalf("sandbox claim policy = %+v", lease.IsolationPolicy)
				}
			}
		})
	}
}

func TestRunnerIsolationClaims(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		report       isolation.Report
		tier         string
		want         int
		requiredTags []string
		stalePolicy  bool
	}{
		{"missing report", nil, "sandbox", http.StatusConflict, nil, false},
		{"sandbox", isolation.Report{"codex": {"sandbox", "native-trusted"}}, "sandbox", http.StatusOK, nil, false},
		{"native cannot sandbox", isolation.Report{"codex": {"native-trusted"}}, "sandbox", http.StatusConflict, nil, false},
		{"trusted", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusOK, nil, false},
		{"trusted missing report", nil, "native-trusted", http.StatusConflict, nil, false},
		{"later heartbeat restores sandbox", isolation.Report{"codex": {"native-trusted"}}, "sandbox", http.StatusConflict, nil, false},
		{"sandbox project selector excludes trusted runner", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusConflict, []string{"sandbox"}, false},
		{"mixed backends", isolation.Report{"codex": {"sandbox"}, "claude": {"native-trusted"}}, "sandbox", http.StatusConflict, nil, false},
		{"probe failed", isolation.Report{"codex": {}}, "sandbox", http.StatusConflict, nil, false},
		{"stale permissive policy cannot bypass current selector", isolation.Report{"codex": {"native-trusted"}}, "native-trusted", http.StatusConflict, []string{"sandbox"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := hubfixture.Open(t, hubfixture.Config{})
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					f := hubfixture.NewNative(t, service, organization, "Example project")
					r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					r.Redemption.BackendIsolation = test.report
					r.Enroll(t)
					r.ReportCapacity(t)
					issue := f.Create(t, "queued")
					settings := map[string]any{"expected_revision": 1, "display_name": "Runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.Project.ID}, "isolation_tier": test.tier}
					hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", hubfixture.AdminToken, settings), http.StatusOK)
					permissive := hubfixture.Policy()
					if test.stalePolicy {
						hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", permissive)
					}
					descriptor := hubfixture.Policy()
					descriptor.Requirements = policy.Requirements{RequiredTags: test.requiredTags}
					descriptor.ID = ""
					descriptor = descriptor.WithID()
					hubfixture.ApprovePolicy(t, f.Hub, f.Base+"/policy", descriptor)
					claimPolicy := descriptor.ID
					if test.stalePolicy {
						claimPolicy = permissive.ID
					}
					claim := tracker.NativeClaim{PolicyID: claimPolicy, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
					hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim), test.want)
					if test.name == "later heartbeat restores sandbox" {
						r.Redemption.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
						r.ReportCapacity(t)
						hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim), http.StatusOK)
					}
				})
			}
		})
	}
}

func TestRunnerProjectDiagnosticsIsolationPlatform(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, goos, tier string
		unsupported      bool
		misreported      bool
	}{
		{"windows sandbox", "windows", isolation.Sandbox, true, false},
		{"windows sandbox misreported", "windows", isolation.Sandbox, true, true},
		{"windows full access", "windows", isolation.NativeTrusted, false, false},
		{"darwin sandbox", "darwin", isolation.Sandbox, false, false},
		{"linux sandbox", "linux", isolation.Sandbox, false, false},
		{"legacy unspecified platform", "", isolation.Sandbox, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.Redemption.IsolationTier = test.tier
			r.Redemption.OS = test.goos
			if test.goos == "windows" {
				r.Redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted}}
			}
			if test.misreported {
				r.Redemption.BackendIsolation = isolation.Report{"codex": {isolation.NativeTrusted, isolation.Sandbox}}
			}
			r.Enroll(t)
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{
				"display_name": r.Redemption.DisplayName, "capacity": r.Redemption.Capacity, "version": "test", "os": test.goos, "backend_isolation": r.Redemption.BackendIsolation,
				"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: r.Redemption.Capacity, ClientLimit: r.Redemption.Capacity, RuntimeLimit: r.Redemption.Capacity, Manageable: true, ObservedAt: f.Hub.Now()},
			}), http.StatusOK)
			response := f.Hub.Request(t, http.MethodGet, f.Base+"/runner-diagnostics?runner_id="+r.Binding.RunnerID, r.Redemption.Credential, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page runnerauth.DiagnosticPage
			hubfixture.Decode(t, response, &page)
			reason := page.Unavailable["isolation_tier"]
			if test.unsupported {
				if !strings.Contains(reason, "sandbox is unavailable on this platform") || !strings.Contains(reason, "native-trusted") {
					t.Fatalf("diagnostics omitted platform reason: %+v", page)
				}
			} else if reason != "" {
				t.Fatalf("supported platform tier reported unavailable: %+v", page)
			}
			issue := f.Create(t, "queued")
			claim := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: issue.WorkItemID, MachineID: r.Binding.MachineID, SessionID: "claim", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			want := http.StatusOK
			if test.unsupported {
				want = http.StatusConflict
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, claim), want)
		})
	}
}

func TestRunnerUpdateMCP(t *testing.T) {
	t.Parallel()
	for _, fromRelease := range []bool{false, true} {
		t.Run(fmt.Sprintf("from release %t", fromRelease), func(t *testing.T) {
			f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
			r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.Enroll(t)
			now := time.Now().UTC()
			observation := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "0.117.63", AvailableObservedAt: now, ObservedAt: now, Running: runnerauth.BuildEvidence{Version: "0.117.62", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}}
			observation.Revision = observation.BuildRevision()
			heartbeat := f.Hub.Request(t, http.MethodPost, f.Base+"/machines/"+string(r.Binding.MachineID)+"/heartbeat", r.Redemption.Credential, map[string]any{"display_name": "isolated-update", "capacity": 1, "version": observation.Running.Version, "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": observation, "backend_isolation": r.Redemption.BackendIsolation})
			hubfixture.RequireStatus(t, heartbeat, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			hubfixture.Decode(t, heartbeat, &snapshot)
			var sessionID string
			send := func(body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(f.Project.OrganizationID)+"/mcp", bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+hubfixture.AdminToken)
				request.Header.Set("Content-Type", "application/json")
				if sessionID != "" {
					request.Header.Set("Mcp-Session-Id", sessionID)
					request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
				}
				response := httptest.NewRecorder()
				f.Hub.Handler().ServeHTTP(response, request)
				return response
			}
			connect := func() {
				t.Helper()
				sessionID = ""
				initialized := send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "isolated-update", "version": "1"}}})
				hubfixture.RequireStatus(t, initialized, http.StatusOK)
				sessionID = initialized.Header().Get("Mcp-Session-Id")
				if sessionID == "" {
					t.Fatal("missing MCP session")
				}
				hubfixture.RequireStatus(t, send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}), http.StatusAccepted)
			}
			connect()
			call := func(t *testing.T, key string, change json.RawMessage, wantCode, wantMessage string) runnerauth.UpdateView {
				t.Helper()
				arguments := map[string]any{"project_id": string(f.Project.ID), "runner_id": r.Binding.RunnerID, "request_id": key, "change": change}
				response := send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": operatortool.UpdateApply, "arguments": arguments}})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var reply struct {
					Result struct {
						IsError bool                    `json:"isError"`
						Content []struct{ Text string } `json:"content"`
					} `json:"result"`
				}
				hubfixture.Decode(t, response, &reply)
				if len(reply.Result.Content) != 1 || reply.Result.IsError != (wantCode != "") {
					t.Fatalf("MCP reply=%s", response.Body)
				}
				content := []byte(reply.Result.Content[0].Text)
				if wantCode != "" {
					var failure operatortool.ConflictError
					if err := json.Unmarshal(content, &failure); err != nil || failure.Code != wantCode || !strings.Contains(failure.Message, wantMessage) || wantCode == "revision_conflict" && failure.CurrentRevision != snapshot.Revision {
						t.Fatalf("refusal=%s error=%v", content, err)
					}
					return runnerauth.UpdateView{}
				}
				var result struct {
					Status  string                `json:"status"`
					Receipt runnerauth.UpdateView `json:"receipt"`
				}
				if err := json.Unmarshal(content, &result); err != nil || result.Status != "succeeded" || result.Receipt.Desired == nil {
					t.Fatalf("acceptance=%s error=%v", content, err)
				}
				return result.Receipt
			}
			selection := runnerupdate.Change{ExpectedRevision: snapshot.Revision, ExpectedBuildRevision: observation.Revision, Service: "detent", Version: "0.117.63", Release: true, FromRelease: fromRelease}
			for _, test := range []struct {
				name, message string
				change        func(*runnerupdate.Change)
			}{
				{"stale runner", "runner revision", func(c *runnerupdate.Change) { c.ExpectedRevision++ }},
				{"stale build", "build revision", func(c *runnerupdate.Change) { c.ExpectedBuildRevision = strings.Repeat("e", 64) }},
				{"different available release", "selected version", func(c *runnerupdate.Change) { c.Version = "0.117.64" }},
			} {
				t.Run(test.name, func(t *testing.T) {
					change := selection
					test.change(&change)
					raw, err := json.Marshal(change)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					delete(fields, "confirm")
					raw, err = json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					call(t, test.name, raw, "revision_conflict", test.message)
				})
			}
			fromReleaseField := ""
			if fromRelease {
				fromReleaseField = `,"from_release":true`
			}
			raw := json.RawMessage(fmt.Sprintf(`{"expected_revision":%d,"expected_build_revision":%q,"service":"detent","version":"0.117.63","release":true%s}`, snapshot.Revision, observation.Revision, fromReleaseField))
			accepted := call(t, "fresh-selection", raw, "", "")
			if accepted.Revision != snapshot.Revision+1 || accepted.Status != "requested" || accepted.Desired.Version != "0.117.63" || accepted.Desired.ExpectedBuildRevision != observation.Revision || accepted.Desired.FromRelease != fromRelease {
				t.Fatalf("accepted update=%+v", accepted)
			}
			reordered := json.RawMessage(fmt.Sprintf(`{"from_release":%t,"release":true,"version":"0.117.63","service":"detent","expected_build_revision":%q,"expected_revision":%d}`, fromRelease, observation.Revision, snapshot.Revision))
			for _, reconnect := range []bool{false, true} {
				if reconnect {
					connect()
				}
				replayed := call(t, "fresh-selection", reordered, "", "")
				if replayed.Revision != accepted.Revision || *replayed.Desired != *accepted.Desired {
					t.Fatal("normalized retry duplicated update work")
				}
				changed := bytes.ReplaceAll(raw, []byte("0.117.63"), []byte("0.117.64"))
				call(t, "fresh-selection", changed, "idempotency_conflict", "different content")
			}
			snapshot.Revision = accepted.Revision
			call(t, "fresh-but-unsettled", raw, "revision_conflict", "has not settled")
			var current runnerauth.UpdateView
			response := f.Hub.Request(t, http.MethodGet, r.IdentityPath()+"/update", hubfixture.AdminToken, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			hubfixture.Decode(t, response, &current)
			if current.Revision != accepted.Revision || *current.Desired != *accepted.Desired || current.Observation.Receipt != nil {
				t.Fatalf("request acceptance changed running evidence or duplicated work: %+v", current)
			}
			receipt, err := json.Marshal(accepted)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("isolated normalized input=%s accepted receipt=%s", raw, receipt)
		})
	}
}
