package hubserver

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedRunnerUpdateReadKeyAuthority(t *testing.T) {
	for _, test := range []struct{ deployment, role string }{
		{"dedicated", "owner"}, {"shared", "owner"}, {"dedicated", "member"}, {"shared", "member"},
	} {
		t.Run(test.deployment+"/"+test.role, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			if test.deployment == "shared" {
				shared = newHostedSharedFixture(t)
				f = shared.hostedSecurityFixture
			} else {
				f = newHostedSecurityFixture(t)
			}
			owner := f.user(t, "reader", test.role, "reader@example.test", "write", "")
			f.service.config.generateToken = apikey.GenerateToken
			f.grant(t, owner, true, true)
			credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: owner.identity.Hosted, Email: owner.identity.Email}, apikey.HashToken(owner.token))
			if err != nil {
				t.Fatal(err)
			}
			operatorSQL(t, f, "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT 'prj_update_other',organization_id,'other',profile,states_json,created_at FROM projects WHERE id=?", f.project)
			operatorSQL(t, f, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner) VALUES (?,'org_security','prj_update_other',1,1) ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=1,manage_runner=1", owner.identity.Subject)
			operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,'org_security','prj_update_other') ON CONFLICT DO NOTHING", credential.ID)
			binding := runnerauth.NewBinding()
			value, err := f.service.createRunnerEnrollmentCommand(t.Context(), nativeScope{organization: "org_security", credential: credential}, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 60})
			if err != nil {
				t.Fatal(err)
			}
			enrollment := value.(runnerauth.Enrollment)
			runnerToken, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: binding, Credential: runnerToken, Hostname: "update-host", Version: "1.2.3", Capacity: 1, BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}}
			if test.deployment == "shared" {
				body, err := json.Marshal(redemption)
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: http.MethodPost, target: "/organizations/org_security/api/v2/organizations/org_security/runner-enrollments/redeem", bearer: enrollment.Token, body: string(body)}), http.StatusCreated)
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
			}
			path := "/api/v2/organizations/org_security/runners/" + binding.RunnerID + "/update"
			for _, keyScope := range []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite} {
				t.Run(string(keyScope), func(t *testing.T) {
					key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "update-reader-" + string(keyScope), Scope: apiScopeOperator, Issuer: &credential, KeyScope: keyScope, ProjectAccess: hostedProjectsAll})
					if err != nil {
						t.Fatal(err)
					}
					read := func(target string) *httptest.ResponseRecorder {
						if test.deployment == "shared" {
							return shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, method: http.MethodGet, target: "/organizations/org_security" + target, bearer: key.Token})
						}
						return performHubAPIRequest(t, f.service, http.MethodGet, target, key.Token, nil)
					}
					response := read(path)
					requireNativeStatus(t, response, http.StatusOK)
					var view runnerauth.UpdateView
					decodeHubResponse(t, response, &view)
					if view.RunnerID != binding.RunnerID || view.Status != "unavailable" {
						t.Fatalf("update receipt=%+v", view)
					}
					if keyScope == apikey.ScopeRead {
						original := key
						key, err = f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "selected-update-reader", Scope: apiScopeOperator, Issuer: &credential, KeyScope: keyScope, ProjectAccess: hostedProjectsSelected, ProjectIDs: []string{string(f.project)}})
						if err != nil {
							t.Fatal(err)
						}
						requireNativeStatus(t, read(path), http.StatusForbidden)
						key = original
					}
					requireNativeStatus(t, read(strings.Replace(path, "org_security", "org_other", 1)), http.StatusNotFound)
					f.grant(t, owner, true, false)
					want := http.StatusOK
					if test.role == "member" {
						want = http.StatusNotFound
					}
					requireNativeStatus(t, read(path), want)
					f.grant(t, owner, true, true)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET expires_at=? WHERE token_hash=?", formatHubTime(time.Now().Add(-time.Minute)), apikey.HashToken(key.Token)); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, read(path), http.StatusNotFound)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET expires_at=NULL WHERE token_hash=?", apikey.HashToken(key.Token)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE token_hash=?", formatHubTime(time.Now()), apikey.HashToken(key.Token)); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, read(path), http.StatusNotFound)
				})
			}
		})
	}
}

func TestRunnerAutomaticallyFollowsHub(t *testing.T) {
	for _, test := range []struct {
		name, hub, runner, prior, wantVersion  string
		supported, manual, unpublished, failed bool
		want                                   bool
	}{
		{name: "older runner", hub: "v1.2.4", runner: "1.2.3", supported: true, want: true},
		{name: "runner ahead", hub: "v1.2.4", runner: "1.2.5", supported: true},
		{name: "installed operator build", hub: "v1.2.4", runner: "operator-landed-abcdef123456", supported: true},
		{name: "unpublished release", hub: "v1.2.4", runner: "1.2.3", supported: true, unpublished: true},
		{name: "update failed", hub: "v1.2.4", runner: "1.2.3", supported: true, failed: true, want: true},
		{name: "matching release", hub: "v1.2.4", runner: "1.2.4", supported: true},
		{name: "development Hub", hub: "dev", runner: "1.2.3", supported: true},
		{name: "missing update owner", hub: "v1.2.4", runner: "1.2.3"},
		{name: "operator request retained", hub: "v1.2.4", runner: "1.2.3", supported: true, manual: true, want: true},
		{name: "uncertain older request superseded", hub: "v1.2.4", runner: "1.2.1", prior: "uncertain", supported: true, want: true},
		{name: "refused older request superseded", hub: "v1.2.4", runner: "1.2.1", prior: "refused", supported: true, want: true},
		{name: "uncertain newer operator request retained", hub: "v1.2.4", runner: "1.2.1", prior: "uncertain", wantVersion: "1.2.5", supported: true, manual: true, want: true},
		{name: "draining older request retained", hub: "v1.2.4", runner: "1.2.1", prior: "draining", wantVersion: "1.2.2", supported: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Version: "dev", RunnerReleaseClient: &runnerReleaseFixture{}})
			f := newNativeFixture(t, service, "", "follow-hub")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			r.enroll(t)
			now := service.config.now()
			build := runnerauth.BuildEvidence{Version: test.runner, Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			report := &runnerauth.UpdateObservation{Discovery: "unknown", Protocol: 1, Service: "detent", Supported: test.supported, Running: build, ObservedAt: now}
			report.Revision = report.BuildRevision()
			send := func() runnerauth.RoutingSnapshot {
				response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": report.Running.Version, "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": report, "backend_isolation": r.redemption.BackendIsolation})
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				return snapshot
			}
			if test.manual {
				report.Discovery = "available"
				report.AvailableVersion = "1.2.5"
				report.AvailableObservedAt = now
				report.Revision = report.BuildRevision()
				snapshot := send()
				request := runnerUpdateChange{ExpectedRevision: snapshot.Revision, ExpectedBuildRevision: report.Revision, Service: "detent", Version: "1.2.5", Release: true, Confirm: true, IdempotencyKey: "operator-selection"}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request), http.StatusAccepted)
			}
			if test.prior != "" {
				service.runnerPublishedReleases.Store("v1.2.2/linux/amd64", true)
				service.config.Version = "v1.2.2"
				earlier := send().Routing.UpdateRequest
				earlierVersion := "1.2.2"
				if test.manual {
					earlierVersion = "1.2.5"
				}
				if earlier == nil || earlier.Version != earlierVersion {
					t.Fatalf("earlier request=%+v", earlier)
				}
				report.Receipt = &runnerauth.UpdateReceipt{Request: *earlier, Status: test.prior, FailureReason: "Update was interrupted before completion", ObservedAt: now}
				if test.prior == "draining" {
					report.Receipt.FailureReason = ""
				}
			}
			if !test.unpublished {
				service.runnerPublishedReleases.Store("v1.2.4/linux/amd64", true)
			}
			service.config.Version = test.hub
			snapshot := send()
			if snapshot.TargetRunnerVersion != "1.2.4" && !test.unpublished && test.hub == "v1.2.4" {
				t.Fatalf("heartbeat target = %q", snapshot.TargetRunnerVersion)
			}
			if test.unpublished && snapshot.TargetRunnerVersion != "" {
				t.Fatal("advertised unpublished target")
			}
			request := snapshot.Routing.UpdateRequest
			wantState := "active"
			if test.want && !test.manual {
				wantState = "draining"
			}
			if (request != nil) != test.want || snapshot.Routing.State != wantState {
				t.Fatalf("routing=%+v", snapshot)
			}
			if !test.want {
				return
			}
			wantVersion := cmp.Or(test.wantVersion, "1.2.4")
			if request.FollowHub == test.manual || !test.manual && (request.Version != wantVersion || request.ExpectedBuildRevision != report.Revision || request.Validate() != nil) {
				t.Fatalf("request=%+v", request)
			}
			if test.wantVersion != "" {
				return
			}
			repeated := send()
			if *repeated.Routing.UpdateRequest != *request || repeated.Revision != snapshot.Revision {
				t.Fatal("heartbeat changed the update identity")
			}
			if test.failed {
				report.Receipt = &runnerauth.UpdateReceipt{Request: *request, Status: "refused", FailureReason: "Update download failed", ObservedAt: now}
				send()
				stored, err := readRunner(t.Context(), service.database.db, f.project.OrganizationID, r.binding.RunnerID, service.config.now())
				if err != nil || stored.UpdateView(service.config.now()).Status != "refused" || stored.Update.Receipt.FailureReason != "Update download failed" {
					t.Fatalf("failed update not reported: %+v %v", stored.Update, err)
				}
				return
			}
			applied := build
			applied.Version = request.Version
			applied.Commit = strings.Repeat("c", 40)
			applied.SHA256 = strings.Repeat("d", 64)
			applied.VerifiedRelease = true
			report.Receipt = &runnerauth.UpdateReceipt{Request: *request, Status: "restart_requested", Applied: &applied, ObservedAt: now}
			send()
			report.Running = applied
			report.Receipt.Status = "running"
			report.Revision = report.BuildRevision()
			send()
			observedAt := service.config.now()
			stored, err := readRunner(t.Context(), service.database.db, f.project.OrganizationID, r.binding.RunnerID, observedAt)
			if test.manual {
				if err != nil || stored.UpdateRequest == nil || stored.UpdateRequest.FollowHub || stored.UpdateRequest.Version != "1.2.5" {
					t.Fatalf("completed operator request suppressed Hub following: %+v %v", stored.UpdateRequest, err)
				}
				return
			}
			if err != nil || stored.UpdateView(observedAt).Status != "running" || !stored.Update.Running.VerifiedRelease {
				t.Fatalf("running receipt=%+v error=%v", stored.Update, err)
			}
		})
	}
}

func TestRunnerUpdateApplication(t *testing.T) {
	f := newNativeFixture(t, nil, "", "runner-update")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.enroll(t)
	now := time.Now().UTC()
	build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "private_patched_source", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
	report := &runnerauth.UpdateObservation{Discovery: "available", Protocol: 1, Service: "detent", Supported: true, AvailableVersion: "1.2.4", AvailableObservedAt: now, Running: build, ObservedAt: now}
	report.Revision = report.BuildRevision()
	send := func(evidence *runnerauth.UpdateObservation, protocol int) runnerauth.RoutingSnapshot {
		version := "1.2.3"
		if evidence != nil {
			version = evidence.Running.Version
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": version, "os": "linux", "architecture": "amd64", "protocol_major": protocol, "update": evidence, "backend_isolation": r.redemption.BackendIsolation})
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot runnerauth.RoutingSnapshot
		decodeHubResponse(t, response, &snapshot)
		return snapshot
	}
	snapshot := send(report, 2)
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	issue := f.create(t, "active update work")
	claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "active-update", TTLSeconds: 300, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	leaseResponse := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, claim)
	requireNativeStatus(t, leaseResponse, http.StatusOK)
	var active tracker.NativeLease
	decodeHubResponse(t, leaseResponse, &active)

	read := func() runnerauth.UpdateView {
		var view runnerauth.UpdateView
		response := performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/update", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &view)
		return view
	}
	view := read()
	if view.Status != "ready" || view.Observation.Running.Source != "private_patched_source" || view.Observation.Running.VerifiedRelease {
		t.Fatalf("initial view=%+v", view)
	}
	request := runnerUpdateChange{ExpectedRevision: snapshot.Revision, ExpectedBuildRevision: report.Revision, Service: "detent", Version: "1.2.4", Release: true, Confirm: true, IdempotencyKey: "selected-release"}
	staleBuild := request
	staleBuild.ExpectedBuildRevision = strings.Repeat("e", 64)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, staleBuild), http.StatusConflict)
	unavailable := *report
	unavailable.Supported = false
	unavailable.Revision = unavailable.BuildRevision()
	send(&unavailable, 2)
	if got := read(); got.Status != "unavailable" || got.Observation == nil || got.Observation.Protocol != 1 {
		t.Fatalf("missing installed owner=%+v", got)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request), http.StatusServiceUnavailable)
	send(report, 2)
	noConfirm := request
	noConfirm.Confirm = false
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, noConfirm), http.StatusPreconditionRequired)
	response := performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusAccepted)
	replay := performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusAccepted)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("changed idempotent receipt")
	}
	view = read()
	if view.Status != "requested" || view.Desired == nil || view.Observation.Receipt != nil {
		t.Fatalf("queued receipt=%+v", view)
	}
	for _, test := range []struct {
		name       string
		change     func(*runnerUpdateChange)
		credential string
		path       string
		want       int
	}{
		{"changed retry", func(c *runnerUpdateChange) { c.Version = "1.2.5" }, testHubAdminToken, r.identityPath() + "/update/apply", http.StatusConflict},
		{"stale request", func(c *runnerUpdateChange) { c.IdempotencyKey = "stale" }, testHubAdminToken, r.identityPath() + "/update/apply", http.StatusConflict},
		{"worker denied", func(c *runnerUpdateChange) {}, r.redemption.Credential, r.identityPath() + "/update/apply", http.StatusForbidden},
		{"cross organization", func(c *runnerUpdateChange) {}, testHubAdminToken, "/api/v2/organizations/org_other/runners/" + r.binding.RunnerID + "/update/apply", http.StatusNotFound},
		{"shared hub service denied", func(c *runnerUpdateChange) { c.Service = "hub" }, testHubAdminToken, r.identityPath() + "/update/apply", http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := request
			test.change(&copy)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, test.path, test.credential, copy), test.want)
		})
	}
	snapshot = send(report, 2)
	if snapshot.Routing.UpdateRequest == nil || *snapshot.Routing.UpdateRequest != *view.Desired || snapshot.Routing.CapacityLimit != 2 || snapshot.Routing.State != "active" {
		t.Fatalf("delivery=%+v", snapshot)
	}
	applied := build
	applied.Version = "1.2.4"
	applied.Commit = strings.Repeat("c", 40)
	applied.SHA256 = strings.Repeat("d", 64)
	applied.Source = "release"
	applied.VerifiedRelease = true
	report.Receipt = &runnerauth.UpdateReceipt{Request: *view.Desired, Status: "applied", Applied: &applied, ObservedAt: now}
	send(report, 2)
	if got := read(); got.Status != "applied" || got.Observation.Running.Version == got.Desired.Version {
		t.Fatalf("premature running receipt=%+v", got)
	}
	report.Running = applied
	report.Receipt.Status = "running"
	report.Revision = report.BuildRevision()
	send(report, 2)
	if got := read(); got.Status != "running" || !got.Observation.Running.VerifiedRelease {
		t.Fatalf("running receipt=%+v", got)
	}
	report.Receipt.Request.ID = "foreign"
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "1.2.4", "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": json.RawMessage(raw)}), http.StatusUnprocessableEntity)
	report.Receipt.Request.ID = view.Desired.ID
	report.Running.Source = "private_patched_source"
	report.Running.VerifiedRelease = false
	report.Revision = report.BuildRevision()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "1.2.4", "os": "linux", "architecture": "amd64", "protocol_major": 2, "update": report}), http.StatusUnprocessableEntity)
	send(nil, 1)
	if got := read(); got.Status != "unavailable" || got.Observation != nil {
		t.Fatalf("older runner owner=%+v", got)
	}
	request.IdempotencyKey = "absent-owner"
	request.ExpectedRevision = view.Revision
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, r.identityPath()+"/update/apply", testHubAdminToken, request), http.StatusServiceUnavailable)
	stored, err := readRunner(t.Context(), f.service.database.db, f.project.OrganizationID, r.binding.RunnerID, f.service.config.now())
	if err != nil || len(stored.Leases) != 1 || stored.Leases[0].ID != active.ID {
		t.Fatalf("active lease changed: %+v error=%v", stored.Leases, err)
	}
	var events int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE operation=?", "runner_update "+r.binding.RunnerID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("audit count=%d error=%v", events, err)
	}
}
