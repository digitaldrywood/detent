package hubserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAPITokensAreHashedScopedRotatableAndRedacted(t *testing.T) {
	t.Parallel()

	const (
		adminToken   = "hub-admin-token-that-is-never-persisted-in-plaintext"
		workerToken  = "hub-worker-token-that-is-never-persisted-in-plaintext"
		rotatedToken = "hub-rotated-token-that-is-never-persisted-in-plaintext"
	)
	generated := []string{workerToken, rotatedToken}
	var generatedIndex int
	var logs conversationLogSink
	admin := postgresStartupTemplate.Database(t)
	config := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	config.Hosted = nil
	config.ListenAddress = "127.0.0.1:0"
	config.InitialAdminToken = []byte(adminToken)
	config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	config.Content = (&changeObjectFixture{}).call
	config.SecretKeys = secretTestKeys(t, "1", "1")
	config.newTokenID = func() string { return "worker-token-id" }
	config.generateToken = func() (string, error) {
		token := generated[generatedIndex]
		generatedIndex++
		return token, nil
	}
	service, err := Open(t.Context(), config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	var storedHash string
	var fingerprint string
	if err := service.config.TenantRepository.ReadTenant(t.Context(), service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		credential, err := r.AccessCredential(t.Context(), bootstrapTokenID)
		storedHash, fingerprint = credential.Hash, credential.Fingerprint
		return err
	}); err != nil {
		t.Fatalf("read initial token: %v", err)
	}
	if storedHash != apikey.HashToken(adminToken) || fingerprint != hubapi.TokenFingerprint(storedHash) {
		t.Fatalf("stored token metadata = hash %q fingerprint %q", storedHash, fingerprint)
	}
	if strings.Contains(storedHash, adminToken) || strings.Contains(fingerprint, adminToken) {
		t.Fatal("initial token plaintext was persisted")
	}

	unauthorized := performHubAPIRequest(t, service, http.MethodGet, "/health", "wrong-secret-token", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d body = %s", unauthorized.Code, unauthorized.Body.String())
	}
	maintenanceConfig := service.config
	maintenanceConfig.CredentialMaintenance = true
	maintenanceConfig.Hosted = &HostedConfig{OrganizationID: string(service.accessOrganization()), WorkOSOrganizationID: "provider_api", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
	deployment, generation := maintenanceConfig.Hosted.deployment()
	if err := service.config.TenantRepository.WriteTenant(t.Context(), service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: service.accessOrganization(), ProviderID: maintenanceConfig.Hosted.WorkOSOrganizationID, Deployment: deployment, PublicURL: maintenanceConfig.Hosted.PublicURL, AllocationGeneration: generation})
	}); err != nil {
		t.Fatalf("bind private credential maintenance: %v", err)
	}
	maintenance, err := Open(t.Context(), maintenanceConfig)
	if err != nil {
		t.Fatalf("open private credential maintenance: %v", err)
	}
	t.Cleanup(func() {
		if err := maintenance.Close(); err != nil {
			t.Error(err)
		}
	})
	createdResponse := performHubAPIRequest(t, maintenance, http.MethodPost, "/api/v1/tokens", adminToken, map[string]any{"name": "Worker", "scope": "worker"})
	if createdResponse.Code != http.StatusCreated || createdResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create token status = %d headers = %v body = %s", createdResponse.Code, createdResponse.Header(), createdResponse.Body.String())
	}
	var created hubapi.TokenResponse
	decodeHubResponse(t, createdResponse, &created)
	if created.Token != workerToken || created.Scope != hubapi.APIScopeWorker {
		t.Fatalf("created token = %#v", created)
	}
	if err := service.config.TenantRepository.ReadTenant(t.Context(), service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		credential, err := r.AccessCredential(t.Context(), created.ID)
		storedHash = credential.Hash
		return err
	}); err != nil {
		t.Fatalf("read worker token hash: %v", err)
	}
	if storedHash != apikey.HashToken(workerToken) || strings.Contains(storedHash, workerToken) {
		t.Fatalf("stored worker token hash = %q", storedHash)
	}

	if response := performHubAPIRequest(t, service, http.MethodGet, "/health", workerToken, nil); response.Code != http.StatusOK {
		t.Fatalf("worker read status = %d body = %s", response.Code, response.Body.String())
	}
	if response := performHubAPIRequest(t, maintenance, http.MethodPost, "/api/v1/tokens", workerToken, map[string]any{"name": "Denied", "scope": "worker"}); response.Code != http.StatusForbidden {
		t.Fatalf("worker admin status = %d body = %s", response.Code, response.Body.String())
	}
	rotatedResponse := performHubAPIRequest(t, maintenance, http.MethodPost, "/api/v1/tokens/"+created.ID+"/rotate", adminToken, map[string]any{})
	if rotatedResponse.Code != http.StatusOK || rotatedResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate token status = %d body = %s", rotatedResponse.Code, rotatedResponse.Body.String())
	}
	var rotated hubapi.TokenResponse
	decodeHubResponse(t, rotatedResponse, &rotated)
	if rotated.Token != rotatedToken || rotated.RotatedAt.IsZero() {
		t.Fatalf("rotated token = %#v", rotated)
	}
	if response := performHubAPIRequest(t, service, http.MethodGet, "/health", workerToken, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("old worker token status = %d, want 401", response.Code)
	}
	if response := performHubAPIRequest(t, service, http.MethodGet, "/health", rotatedToken, nil); response.Code != http.StatusOK {
		t.Fatalf("rotated worker token status = %d body = %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{adminToken, workerToken, rotatedToken, "wrong-secret-token"} {
		if strings.Contains(logs.text(), secret) {
			t.Fatalf("logs contain API token %q", secret)
		}
	}
	if err := service.Close(); err != nil {
		t.Fatalf("close before restart: %v", err)
	}
	if err := maintenance.Close(); err != nil {
		t.Fatalf("close maintenance before restart: %v", err)
	}
	restartConfig := service.config
	previousRepository := restartConfig.TenantRepository
	restartConfig.TenantRepository, restartConfig.AccessRepository, restartConfig.BillingRepository = nil, nil, nil
	restartConfig.InitialAdminToken = []byte("different-startup-token-must-not-undo-rotation")
	restartConfig.Logger = discardLogger()
	restarted, err := Open(t.Context(), restartConfig)
	if err != nil {
		t.Fatalf("restart Hub: %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Fatalf("close restarted Hub: %v", err)
		}
	})
	if response := performHubAPIRequest(t, restarted, http.MethodGet, "/health", rotatedToken, nil); response.Code != http.StatusOK {
		t.Fatalf("rotated token after restart status = %d body = %s", response.Code, response.Body.String())
	}
	if restarted.config.Postgres.Connection.Database != service.config.Postgres.Connection.Database {
		t.Fatal("token restart changed PostgreSQL database")
	}
	if restarted.config.TenantRepository == previousRepository || restarted.config.AccessRepository == service.config.AccessRepository || restarted.config.BillingRepository == service.config.BillingRepository {
		t.Fatal("token restart retained a closed repository")
	}
	if err := restarted.config.TenantRepository.ReadTenant(t.Context(), restarted.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		credential, err := r.AccessCredential(t.Context(), bootstrapTokenID)
		storedHash = credential.Hash
		return err
	}); err != nil {
		t.Fatalf("read bootstrap token after restart: %v", err)
	}
	if storedHash != apikey.HashToken(adminToken) {
		t.Fatalf("restart replaced bootstrap token hash = %q", storedHash)
	}
}

func newRetainedWorkListFixture(t *testing.T) (nativeFixture, runnerFixture, []tracker.NativeWorkItemID, policy.Descriptor) {
	t.Helper()
	return newRetainedWorkListFixtureWith(t, &memoryRunDetails{})
}

func newRetainedWorkListFixtureWith(t *testing.T, objects *memoryRunDetails) (nativeFixture, runnerFixture, []tracker.NativeWorkItemID, policy.Descriptor) {
	t.Helper()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	clock := &leaseTestClock{value: now}
	f := newNativeFixture(t, newClockedPostgresNativeService(t, Config{now: clock.Now, RunDetails: objects}), "", "Work list authority")
	organization := f.project.OrganizationID
	project := tracker.ProjectID(hubapi.NewID("prj"))
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Retained work list", Profile: "github_compatible"}, States: nativeFixtureStates()})
		if err != nil {
			return err
		}
		record.Settings.Repository, record.Settings.CheckoutRepository, record.Settings.RepositoryEnabled = "example-org/example-project", "example-org/example-project", true
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.project = tracker.NativeProject{OrganizationID: organization, ID: project, Name: "Retained work list", Profile: "github_compatible", States: nativeFixtureStates()}
	f.base = "/api/v2/organizations/" + string(organization) + "/projects/" + string(project)
	ids := []tracker.NativeWorkItemID{"wi_list_first", "wi_list_second", "wi_list_third", "wi_list_unranked"}
	source := newCoreNeutralFixture(t, organization, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
	source.add("workflow_states", map[string]any{"id": 1, "project_id": project, "detent_state": "In Progress"})
	source.add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
	stamp := now.Format(time.RFC3339Nano)
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-reporter"}
	for index, id := range ids {
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: id, Number: index + 1, Revision: 1, Profile: "github_compatible"}, Title: "Retained work item", Body: "Retained issue body", State: "In Progress", Actor: actor, CreatedAt: now, UpdatedAt: now, LastActivityAt: now}
		if index == 2 {
			issue.Body = "Complete issue body"
			issue.Labels = []string{"api"}
			issue.Assignees = []string{"worker-a"}
		}
		issue.ExternalReferences = []tracker.ExternalReference{tracker.GitHubIssueSourceReference("I_"+string(id), fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", index+1))}
		sequence := 1
		if index == 2 {
			sequence = 2
		}
		source.add("issues", map[string]any{"id": 41 + index, "native_id": id, "project_id": project, "number": index + 1, "revision": 1, "event_sequence": sequence, "title": issue.Title, "body": issue.Body, "issue_contract_json": nil, "actor_json": string(historyTestJSON(t, actor)), "archived": 0, "created_at": stamp, "updated_at": stamp, "native_created_at": stamp, "native_updated_at": stamp, "last_activity_at": stamp, "workflow_state_id": 1, "labels_json": string(historyTestJSON(t, issue.Labels)), "assignees_json": string(historyTestJSON(t, issue.Assignees)), "author_login": "example-reporter", "repository_id": 1})
		source.add("collaboration_versions", map[string]any{"project_id": project, "work_item_id": id, "record_id": id, "revision": 1, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
		for event := 1; event <= sequence; event++ {
			kind := "issue.created"
			if index == 2 {
				kind = fmt.Sprintf("event_%d", event)
			}
			source.add("collaboration_events", map[string]any{"id": fmt.Sprintf("evt_%s_%d", id, event), "project_id": project, "work_item_id": id, "sequence": event, "type": kind, "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: 1})), "event_revision": 1, "recorded_at": stamp})
		}
		if index < 3 {
			for scopeIndex, scope := range []string{"fleet", string(project)} {
				priority := tracker.QueuePriorityUrgent
				if index == 0 {
					priority = tracker.QueuePriorityLow
				}
				source.add("queue_entries", map[string]any{"id": index*2 + scopeIndex + 1, "issue_id": 41 + index, "scope": scope, "state": "In Progress", "rank": []string{"m", "z", "a"}[index], "priority_override": priority, "created_at": stamp, "updated_at": stamp})
			}
		}
	}
	count, err := ImportCoreWorkItems(t.Context(), f.service.config.TenantRepository.(*postgres.Repository), CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: organization, ProjectID: project, Objects: f.service.runDetailStore()})
	if err != nil || count != 4 {
		t.Fatalf("retained work list import=%d error=%v", count, err)
	}
	descriptor := hubTestPolicy()
	approved, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	approved.ProjectID, approved.ApprovedBy, approved.ApprovedAt = project, "credential_owner", now
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.ApproveProjectPolicy(t.Context(), approved, "") }); err != nil {
		t.Fatal(err)
	}
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 8, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "provider_reports": []any{capacityReport(now)}, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: now}, "admission": tracker.NativeAdmissionObservation{RunnerRevision: 1, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: descriptor.ID, WorkflowStates: []string{"In Progress"}}}})
	requireNativeStatus(t, response, http.StatusOK)
	return f, runner, ids, descriptor
}

func TestWorkItemAPIUsesStableFilteredCursorsAndDetailTimeline(t *testing.T) {
	t.Parallel()
	objects := &memoryRunDetails{}
	f, _, ids, _ := newRetainedWorkListFixtureWith(t, objects)
	list := f.base + "/work-items"
	firstPath := list + "?repository=example-org%2Fexample-project&queue_scope=fleet&readiness=ready&priority=urgent&limit=1"
	first := performHubAPIRequest(t, f.service, http.MethodGet, firstPath, testHubAdminToken, nil)
	requireNativeStatus(t, first, http.StatusOK)
	var page tracker.NativeIssuePage
	decodeHubResponse(t, first, &page)
	if len(page.Items) != 1 || page.Items[0].WorkItemID != ids[2] || page.NextCursor == "" {
		t.Fatalf("first page=%+v", page)
	}
	cursor := page.NextCursor
	encodedCursor, err := base64.RawURLEncoding.DecodeString(strings.Split(cursor, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if bytes.Contains(encodedCursor, []byte(id)) {
			t.Fatalf("queue continuation exposed source identifier")
		}
	}
	var protected nativeCursor
	var envelope hubsecrets.Envelope
	if json.Unmarshal(encodedCursor, &protected) != nil || json.Unmarshal([]byte(protected.After), &envelope) != nil || len(envelope.Ciphertext) == 0 || len(envelope.WrappedKey) == 0 {
		t.Fatal("queue continuation is not encrypted")
	}
	secondPath := firstPath + "&cursor=" + url.QueryEscape(cursor)
	second := performHubAPIRequest(t, f.service, http.MethodGet, secondPath, testHubAdminToken, nil)
	requireNativeStatus(t, second, http.StatusOK)
	page = tracker.NativeIssuePage{}
	decodeHubResponse(t, second, &page)
	if len(page.Items) != 1 || page.Items[0].WorkItemID != ids[1] || page.NextCursor != "" {
		t.Fatalf("second page=%+v", page)
	}
	filtered := performHubAPIRequest(t, f.service, http.MethodGet, list+"?label=api&assignee=worker-a&sort=identifier", testHubAdminToken, nil)
	requireNativeStatus(t, filtered, http.StatusOK)
	page = tracker.NativeIssuePage{}
	decodeHubResponse(t, filtered, &page)
	if len(page.Items) != 1 || page.Items[0].WorkItemID != ids[2] {
		t.Fatalf("filtered page=%+v", page)
	}
	unranked := performHubAPIRequest(t, f.service, http.MethodGet, list+"?limit=10", testHubAdminToken, nil)
	requireNativeStatus(t, unranked, http.StatusOK)
	var all tracker.NativeIssuePage
	decodeHubResponse(t, unranked, &all)
	if len(all.Items) != 4 || !slices.ContainsFunc(all.Items, func(item tracker.NativeIssue) bool { return item.WorkItemID == ids[3] && item.Queue == nil }) {
		t.Fatalf("unranked retained item omitted: %+v", all)
	}
	for _, sort := range []string{"updated", "identifier"} {
		response := performHubAPIRequest(t, f.service, http.MethodGet, secondPath+"&sort="+sort, testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	}
	for _, request := range []struct {
		token  string
		status int
	}{{"", http.StatusUnauthorized}, {f.token, http.StatusNotFound}} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, firstPath, request.token, nil), request.status)
	}
	encoded, err := json.Marshal(WorkpadDesired{Phase: "implementation", Body: "Hub API workpad"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: f.project.ID, WorkItemID: ids[2], Key: "workpad-api", Repository: "example-org/example-project", IssueNumber: 3, Kind: string(MutationWorkpad), CreatedAt: f.service.config.now(), Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	detailResponse := performHubAPIRequest(t, f.service, http.MethodGet, list+"/"+string(ids[2])+"?include=workpad", testHubAdminToken, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	var detail tracker.NativeIssue
	decodeHubResponse(t, detailResponse, &detail)
	if detail.WorkItemID != ids[2] || detail.Body != "Complete issue body" || detail.Workpad == nil || detail.Workpad.Body != "Hub API workpad" {
		t.Fatalf("detail=%+v", detail)
	}
	historyPath := list + "/" + string(ids[2]) + "/history?limit=1"
	historyResponse := performHubAPIRequest(t, f.service, http.MethodGet, historyPath, testHubAdminToken, nil)
	requireNativeStatus(t, historyResponse, http.StatusOK)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, historyResponse, &history)
	if len(history.Items) != 1 || history.Items[0].Type != "event_1" || history.NextCursor == "" {
		t.Fatalf("first timeline=%+v", history)
	}
	historyResponse = performHubAPIRequest(t, f.service, http.MethodGet, historyPath+"&cursor="+url.QueryEscape(history.NextCursor), testHubAdminToken, nil)
	requireNativeStatus(t, historyResponse, http.StatusOK)
	history = tracker.Page[tracker.CollaborationEvent]{}
	decodeHubResponse(t, historyResponse, &history)
	if len(history.Items) != 1 || history.Items[0].Type != "event_2" || history.NextCursor != "" {
		t.Fatalf("next timeline=%+v", history)
	}
	saved := f
	saved.token = testHubAdminToken
	changed := false
	objects.beforeRead = func(context.Context) error {
		objects.beforeRead = nil
		changed = true
		writeAdmissionIssue(t, saved, detail, func(issue *tracker.NativeIssue) { issue.Title = "Changed during list hydration" })
		return nil
	}
	defer func() { objects.beforeRead = nil }()
	concurrent := performHubAPIRequest(t, f.service, http.MethodGet, list+"?limit=10", testHubAdminToken, nil)
	requireNativeStatus(t, concurrent, http.StatusOK)
	var current tracker.NativeIssuePage
	decodeHubResponse(t, concurrent, &current)
	if !changed || !current.Partial || len(current.Items) != 3 || slices.ContainsFunc(current.Items, func(issue tracker.NativeIssue) bool { return issue.WorkItemID == ids[2] }) {
		t.Fatalf("concurrent edit failed unrelated list rows or served stale content: %+v", current)
	}
}

func TestClaimNextAPIIsAtomicAndFenced(t *testing.T) {
	t.Parallel()
	f, _, ids, descriptor := newRetainedWorkListFixture(t)
	service := f.service
	now := service.config.now()
	runner := prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.redemption.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
	runner.enroll(t)
	registered := performHubAPIRequest(t, service, http.MethodPost, f.base+"/machines/register", runner.redemption.Credential, map[string]any{"id": runner.binding.MachineID, "hostname": "worker-a", "display_name": "Worker A", "capacity": 10, "version": "v1", "backend_isolation": runner.redemption.BackendIsolation})
	requireNativeStatus(t, registered, http.StatusOK)
	reportedSkill := skills.ProviderSkill{Name: "project-review", Description: "Review this project", Path: "/project/.agents/skills/review/SKILL.md", Scope: "project", Enabled: true, UserInvocable: true, Invocation: "$project-review"}
	assertReportedSkill := func() {
		t.Helper()
		if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			reports, _, err := r.RunnerProjectSkills(t.Context(), f.project.ID, runner.binding.RunnerID, now, 1)
			if err != nil {
				return err
			}
			if len(reports) != 1 || reports[0].Skill != reportedSkill || reports[0].OrganizationID != f.project.OrganizationID || reports[0].ProjectID != f.project.ID || reports[0].RunnerID != runner.binding.RunnerID {
				return fmt.Errorf("persisted project visibility report = %+v", reports)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat := func(capacity int, version string, report bool) *httptest.ResponseRecorder {
		t.Helper()
		fields := map[string]any{"display_name": "Worker A", "capacity": capacity, "version": version}
		if report {
			fields["skills"] = []skills.ProviderSkill{reportedSkill}
			fields["backend_isolation"] = runner.redemption.BackendIsolation
			fields["provider_reports"] = []any{capacityReport(now)}
			fields["capacity_configuration"] = runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: now}
		}
		return performHubAPIRequest(t, service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, fields)
	}
	requireNativeStatus(t, heartbeat(10, "test", true), http.StatusOK)
	assertReportedSkill()
	request := providerClaim(runner, tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: ids[2], Revision: 1}}, "")
	request.WorkItemID = ""
	request.LabelInclude = []string{"api"}
	const attempts = 10
	type claimResult struct {
		status int
		body   []byte
	}
	results := make(chan claimResult, attempts)
	requests := make([]*http.Request, attempts)
	for index := range attempts {
		candidate := request
		candidate.SessionID = fmt.Sprintf("session-%d", index)
		requests[index] = newHubAPIRequest(t, http.MethodPost, f.base+"/claims", runner.redemption.Credential, candidate)
	}
	var wait sync.WaitGroup
	for _, request := range requests {
		wait.Go(func() {
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			results <- claimResult{status: response.Code, body: bytes.Clone(response.Body.Bytes())}
		})
	}
	wait.Wait()
	close(results)
	successes := 0
	var lease tracker.NativeLease
	for result := range results {
		if result.status == http.StatusOK {
			successes++
			if err := json.Unmarshal(result.body, &lease); err != nil {
				t.Fatal(err)
			}
		} else if result.status != http.StatusConflict {
			t.Fatalf("contended claim status=%d body=%s", result.status, result.body)
		}
	}
	if successes != 1 || lease.WorkItemID != ids[2] || lease.FencingToken <= 0 {
		t.Fatalf("claim results = %d successes lease %#v", successes, lease)
	}
	var active int
	if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM leases l JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND l.released_at IS NULL", f.project.OrganizationID, f.project.ID, ids[2]).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active leases = %d, want 1", active)
	}
	event := nativeStartedEvent(lease)
	stale := event
	stale.IdempotencyKey = "stale-start"
	stale.Data.FencingToken++
	eventsPath := f.base + "/work-items/" + string(ids[2]) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventsPath, runner.redemption.Credential, stale), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventsPath, runner.redemption.Credential, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 120}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	other := newNativeFixture(t, service, f.project.OrganizationID, "Other repository scope")
	if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), other.project.ID)
		if err != nil {
			return err
		}
		record.Settings.Repository, record.Settings.CheckoutRepository, record.Settings.RepositoryEnabled = "example-org/other-project", "example-org/other-project", true
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	otherIssue := other.create(t, "Other repository item")
	approveHubTestPolicy(t, service, other.base+"/policy", descriptor)
	scoped := providerClaim(runner, otherIssue, "repository-session")
	scoped.WorkItemID = ""
	scopedResponse := performHubAPIRequest(t, service, http.MethodPost, other.base+"/claims", runner.redemption.Credential, scoped)
	requireNativeStatus(t, scopedResponse, http.StatusOK)
	var repositoryLease tracker.NativeLease
	decodeHubResponse(t, scopedResponse, &repositoryLease)
	if repositoryLease.WorkItemID != otherIssue.WorkItemID {
		t.Fatalf("repository-scoped work item = %s, want %s", repositoryLease.WorkItemID, otherIssue.WorkItemID)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, other.base+"/leases/"+string(repositoryLease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: repositoryLease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	requireNativeStatus(t, heartbeat(4, "v2", false), http.StatusOK)
	assertReportedSkill()
	if err := service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		machine, err := r.Machine(t.Context(), string(runner.binding.MachineID))
		if err != nil {
			return err
		}
		if machine.Capacity != 2 || machine.Version != "v2" {
			return fmt.Errorf("partial heartbeat machine = %#v", machine)
		}
		identity, err := r.RunnerIdentity(t.Context(), runner.binding.RunnerID)
		if err == nil && identity.ReportedCapacity != 4 {
			return fmt.Errorf("partial heartbeat reported capacity = %d, want 4", identity.ReportedCapacity)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, heartbeat(4, "v2", true), http.StatusOK)
	specific := providerClaim(runner, tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: ids[2], Revision: 1}}, "specific-session")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, specific), http.StatusOK)
	mismatch := providerClaim(runner, tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: ids[1], Revision: 1}}, "specific-session")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, mismatch), http.StatusConflict)
}

func TestClaimNextAPIAppliesAuthorizationFiltersBeforeClaim(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	descriptor := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
	var accepted tracker.NativeIssue
	for _, source := range []struct {
		author, token, title string
		labels, assignees    []string
	}{
		{"mallory", "mallory-api-filter-token", "Rejected source", []string{"detent:todo", "hold"}, []string{"worker-b"}},
		{"Alice", "alice-api-filter-token", "Authorized source", []string{"detent:todo", "backend"}, []string{"worker-a"}},
	} {
		seedHubAPIToken(t, f.service, source.author, source.token, hubapi.APIScopeOperator)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", source.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-filter-" + source.author}, Title: source.title, Body: "Complete the requested work." + issueContractTestSections, State: "Todo", Labels: source.labels, Assignees: source.assignees})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		if issue.Actor.PrincipalID != source.author {
			t.Fatalf("source actor=%q want%q", issue.Actor.PrincipalID, source.author)
		}
		if source.author == "Alice" {
			accepted = issue
		}
	}
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	heartbeat := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{
		"display_name": runner.redemption.DisplayName, "capacity": 1, "version": runner.redemption.Version, "backend_isolation": runner.redemption.BackendIsolation,
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now},
	})
	requireNativeStatus(t, heartbeat, http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, tracker.NativeClaim{
		PolicyID: descriptor.ID, MachineID: runner.binding.MachineID, SessionID: "filtered-session", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor,
		Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		Authors:      []string{"alice"}, Assignees: []string{"worker-a"}, LabelInclude: []string{"detent:todo", "backend"}, LabelExclude: []string{"hold"},
	})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != accepted.WorkItemID {
		t.Fatalf("filtered claim work item=%s want%s", lease.WorkItemID, accepted.WorkItemID)
	}
}

func TestOperatorMutationsAreTypedAuditedAndScoped(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "operator mutation authority")
	service := f.service
	organization := f.project.OrganizationID
	project := tracker.ProjectID(hubapi.NewID("prj"))
	states := nativeFixtureStates()
	for index := range states {
		if !states[index].Terminal {
			states[index].Transitions = append(states[index].Transitions, "Cancelled")
		}
	}
	states[1].Transitions = append(states[1].Transitions, "Human Review")
	states = append(states, tracker.NativeState{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}}, tracker.NativeState{Name: "Human Review", Transitions: []string{"In Progress", "Merging"}}, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"In Progress", "Done"}})
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Compatible project", Profile: "github_compatible", RequireDependencies: true}, States: states})
		if err != nil {
			return err
		}
		stored.Settings.Repository, stored.Settings.CheckoutRepository, stored.Settings.RepositoryEnabled = "example-org/example-project", "example-org/example-project", true
		_, err = w.UpdateProjectRecord(t.Context(), stored)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source := newCoreNeutralFixture(t, organization, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
	source.add("workflow_states", map[string]any{"id": 1, "project_id": project, "detent_state": "Todo"})
	source.add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
	const stamp = "2026-10-01T00:00:00Z"
	actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
	for index, id := range []tracker.NativeWorkItemID{"wi_operator_first", "wi_operator_second"} {
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: id, Number: index + 1, Revision: 1, Profile: "github_compatible"}, Title: "Retained source", Body: "Retained GitHub content", State: "Todo", Actor: actor, CreatedAt: parseTestTime(t, stamp), UpdatedAt: parseTestTime(t, stamp), LastActivityAt: parseTestTime(t, stamp)}
		issue.ExternalReferences = []tracker.ExternalReference{tracker.GitHubIssueSourceReference(fmt.Sprintf("I_retained_%d", index+1), fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", index+1))}
		source.add("issues", map[string]any{"id": 41 + index, "native_id": id, "project_id": project, "number": index + 1, "revision": 1, "event_sequence": 1, "title": issue.Title, "body": issue.Body, "actor_json": string(historyTestJSON(t, actor)), "archived": 0, "created_at": stamp, "updated_at": stamp, "native_created_at": stamp, "native_updated_at": stamp, "last_activity_at": stamp, "workflow_state_id": 1, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-owner", "repository_id": 1})
		source.add("collaboration_versions", map[string]any{"project_id": project, "work_item_id": id, "record_id": id, "revision": 1, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
		source.add("collaboration_events", map[string]any{"id": "event_" + string(id), "project_id": project, "work_item_id": id, "sequence": 1, "type": "issue.created", "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: 1})), "event_revision": 1, "recorded_at": stamp})
	}
	count, err := ImportCoreWorkItems(t.Context(), service.config.TenantRepository.(*postgres.Repository), CoreWorkItemImportSource{BundlePath: source.write(), OrganizationID: organization, ProjectID: project, Objects: service.runDetailStore()})
	if err != nil || count != 2 {
		t.Fatalf("retained compatible import=%d error=%v", count, err)
	}
	seedHubAPIToken(t, service, "operator-token", "operator-secret-token", hubapi.APIScopeOperator)
	const item = tracker.NativeWorkItemID("wi_operator_first")
	const blocker = tracker.NativeWorkItemID("wi_operator_second")
	base := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project)
	mcp := "/api/v2/organizations/" + string(organization) + "/mcp"
	descriptor := hubTestPolicy()
	descriptor.Configuration = &policy.Configuration{Behavior: json.RawMessage(`{"StateMap":{"IsMap":true,"Map":{"In Progress":"detent:in-progress","Done":"detent:done","Cancelled":"detent:cancelled","Human Review":"detent:review","Merging":"detent:merging"}}}`), Prompt: "Follow the approved workflow"}
	descriptor = descriptor.WithID()
	approved, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	approved.ProjectID, approved.ApprovedBy, approved.ApprovedAt = project, "credential_owner", service.config.now()
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.ApproveProjectPolicy(t.Context(), approved, ""); err != nil {
			return err
		}
		for _, id := range []tracker.NativeWorkItemID{item, blocker} {
			priority := int64(tracker.QueuePriorityUrgent)
			if err := w.PutQueueEntry(t.Context(), claimstore.QueueEntry{OrganizationID: organization, ProjectID: project, WorkItemID: id, Scope: string(project), State: "Todo", Rank: string(id), Priority: &priority, CreatedAt: parseTestTime(t, stamp), UpdatedAt: parseTestTime(t, stamp)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	worker := prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate)
	worker.enroll(t)
	call := func(t *testing.T, token, name, key string, fields map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		fields["project_id"], fields["request_id"] = project, key
		if fields["identifier"] == nil {
			fields["identifier"] = item
		}
		return performHubWorkCall(t, service, mcp, token, name, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": fields, "_meta": json.RawMessage(fleetProtocolMeta)}})
	}
	denied := call(t, worker.redemption.Credential, operatortool.OrderItem, "worker-order", map[string]any{"queue_scope": "fleet", "state": "Todo", "rank": "a"})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("worker operator mutation status = %d body = %s", denied.Code, denied.Body.String())
	}
	native := f.create(t, "native-workflow-label")
	nativeLabel := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "native-workflow-label"}, ExpectedRevision: native.Revision, State: "In Progress", Reason: "user_requested", WorkflowLabel: "detent:in-progress"}
	nativeResponse := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(native.WorkItemID)+"/workflow", f.token, nativeLabel)
	requireNativeStatus(t, nativeResponse, http.StatusUnprocessableEntity)
	var nativeLabelRefusal hubapi.NativeError
	decodeHubResponse(t, nativeResponse, &nativeLabelRefusal)
	if nativeLabelRefusal.Code != "invalid_request" {
		t.Fatalf("native workflow label refusal=%+v", nativeLabelRefusal)
	}
	transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "workflow-api"}, ExpectedRevision: 1, State: "In Progress", Reason: "user_requested", WorkflowLabel: "detent:in-progress"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/workflow", f.token, transition), http.StatusNotFound)
	response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/workflow", "operator-secret-token", transition)
	requireNativeStatus(t, response, http.StatusOK)
	var moved tracker.NativeIssue
	decodeHubResponse(t, response, &moved)
	if moved.WorkItemID != item || moved.State != "In Progress" || moved.Revision != 2 {
		t.Fatalf("workflow state = %s revision=%d", moved.State, moved.Revision)
	}
	for _, request := range []struct {
		name, key, token, refusal string
		fields                    map[string]any
	}{
		{operatortool.MoveItem, "workflow-api", "operator-secret-token", "move_item cannot resume an item already in the requested state", map[string]any{"target_state": "In Progress", "expected_revision": "1", "workflow_label": "detent:in-progress"}},
		{operatortool.SetDependency, "dependency-api", "operator-secret-token", "", map[string]any{"operation": "add", "related": blocker, "expected_revision": "2"}},
		{operatortool.OrderItem, "order-api", testHubAdminToken, "", map[string]any{"queue_scope": "fleet", "state": "In Progress", "rank": "rank-a"}},
		{operatortool.SetQueuePriority, "priority-api", testHubAdminToken, "", map[string]any{"queue_scope": "fleet", "state": "In Progress", "queue_priority": "high"}},
	} {
		for range 2 {
			response := call(t, request.token, request.name, request.key, request.fields)
			requireNativeStatus(t, response, http.StatusOK)
			var envelope struct {
				Result struct {
					Failed  bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			decodeHubResponse(t, response, &envelope)
			if request.refusal != "" {
				var refusal hubapi.NativeError
				if !envelope.Result.Failed || len(envelope.Result.Content) != 1 || json.Unmarshal([]byte(envelope.Result.Content[0].Text), &refusal) != nil || refusal.Code != "invalid_request" || !strings.HasPrefix(refusal.Message, request.refusal) {
					t.Fatalf("operator mutation %s with an HTTP idempotency key = %s, want a distinct MCP retry identity refused as %q", request.name, response.Body.String(), request.refusal)
				}
				continue
			}
			if envelope.Result.Failed {
				t.Fatalf("operator mutation %s refused: %s", request.name, response.Body.String())
			}
		}
	}
	admin := nativeAttemptTestDatabase(t, f)
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), project, item)
		if err != nil {
			return err
		}
		if hubapi.StringValue(row.WorkflowStateName) != "In Progress" || row.Revision != 2 {
			return fmt.Errorf("workflow state = %s revision=%d", hubapi.StringValue(row.WorkflowStateName), row.Revision)
		}
		projectEntry, err := r.QueueEntry(t.Context(), project, item, string(project))
		if err != nil {
			return err
		}
		if projectEntry.State != "In Progress" || projectEntry.Priority == nil || *projectEntry.Priority != tracker.QueuePriorityUrgent {
			return fmt.Errorf("workflow erased compatible priority: %+v", projectEntry)
		}
		dispatch, err := r.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), project, item)
		if err != nil {
			return err
		}
		if dispatch.Generation != 1 {
			return fmt.Errorf("workflow dispatch generation=%d, want 1 after replay", dispatch.Generation)
		}
		entry, err := r.QueueEntry(t.Context(), project, item, "fleet")
		if err != nil {
			return err
		}
		if entry.Rank != "rank-a" || entry.Priority == nil || *entry.Priority != tracker.QueuePriorityHigh {
			return fmt.Errorf("queue entry = rank %q priority %v", entry.Rank, entry.Priority)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var dependencies int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_dependencies d JOIN issues i ON i.organization_id=d.organization_id AND i.project_id=d.dependent_project_id AND i.id=d.dependent_issue_id JOIN issues b ON b.organization_id=d.organization_id AND b.project_id=d.blocker_project_id AND b.id=d.blocker_issue_id WHERE d.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND b.project_id=$2 AND b.native_id=$4", organization, project, item, blocker).Scan(&dependencies); err != nil || dependencies != 1 {
		t.Fatalf("dependency count = %d error = %v", dependencies, err)
	}
	var outboxCount int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, item).Scan(&outboxCount); err != nil || outboxCount != 2 {
		t.Fatalf("outbox count = %d error = %v", outboxCount, err)
	}
	var auditCount int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type IN ('dependency.changed','queue.order_changed')", organization, project, item).Scan(&auditCount); err != nil || auditCount != 2 {
		t.Fatalf("operator audit count = %d error = %v", auditCount, err)
	}
	rows, err := admin.QueryContext(t.Context(), "SELECT command_key FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 ORDER BY id", organization, project, item)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, key := range keys {
		var outbound projectstore.GitHubOutbound
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(projectstore.GitHubOutboxWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			var err error
			outbound, err = owner.GitHubOutbound(t.Context(), key)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		encoded, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), outbound.Payload)
		var desired WorkflowLabelDesired
		if err != nil || json.Unmarshal(encoded, &desired) != nil || outbound.Repository != "example-org/example-project" || outbound.IssueNumber != 1 || desired.Clear {
			t.Fatalf("workflow/priority outbound target=%+v desired=%s error=%v", outbound, encoded, err)
		}
		labels[desired.ManagedPrefix+"|"+desired.Label] = true
	}
	if len(labels) != 2 || !labels["detent:|detent:in-progress"] || !labels["priority:|priority:high"] {
		t.Fatalf("workflow and priority labels=%v", labels)
	}
	cycle := call(t, "operator-secret-token", operatortool.SetDependency, "dependency-cycle", map[string]any{"identifier": blocker, "related": item, "operation": "add", "expected_revision": "1"})
	requireNativeStatus(t, cycle, http.StatusOK)
	var refused struct {
		Result struct {
			Failed  bool                      `json:"isError"`
			Failure operatortool.RequestError `json:"structuredContent"`
		} `json:"result"`
	}
	decodeHubResponse(t, cycle, &refused)
	if !refused.Result.Failed || refused.Result.Failure.Code != "invalid_request" || refused.Result.Failure.Message != "Dependencies cannot form a cycle" {
		t.Fatalf("dependency cycle status = %d body = %s", cycle.Code, cycle.Body.String())
	}
	stale := transition
	stale.IdempotencyKey = "workflow-stale"
	stale.State = "Done"
	stale.WorkflowLabel = "detent:done"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/workflow", "operator-secret-token", stale), http.StatusConflict)
	invalid := transition
	invalid.IdempotencyKey, invalid.ExpectedRevision, invalid.State, invalid.WorkflowLabel = "workflow-invalid-label", 2, "Done", "priority:high"
	response = performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/workflow", "operator-secret-token", invalid)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	var invalidLabel hubapi.NativeError
	decodeHubResponse(t, response, &invalidLabel)
	if invalidLabel.Code != "invalid_request" {
		t.Fatalf("workflow label refusal=%+v", invalidLabel)
	}
	for _, test := range []struct {
		name, state, label string
	}{
		{"contradictory managed label", "Done", "detent:in-progress"},
		{"missing approved mapping", "Todo", "detent:todo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := transition
			request.IdempotencyKey, request.ExpectedRevision, request.State, request.WorkflowLabel = test.name, 2, test.state, test.label
			response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/workflow", "operator-secret-token", request)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			var refused hubapi.NativeError
			decodeHubResponse(t, response, &refused)
			if refused.Code != "invalid_request" {
				t.Fatalf("workflow mapping refusal=%+v", refused)
			}
			response = call(t, "operator-secret-token", operatortool.MoveItem, test.name+"-mcp", map[string]any{"target_state": test.state, "expected_revision": "2", "workflow_label": test.label})
			requireNativeStatus(t, response, http.StatusOK)
			var envelope struct {
				Result struct {
					Failed  bool `json:"isError"`
					Failure struct {
						Code string `json:"code"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			decodeHubResponse(t, response, &envelope)
			if !envelope.Result.Failed || envelope.Result.Failure.Code != "invalid_request" {
				t.Fatalf("workflow mapping tool refusal: %s", response.Body.String())
			}
		})
	}
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, item).Scan(&outboxCount); err != nil || outboxCount != 2 {
		t.Fatalf("refused workflow wrote outbound count=%d error=%v", outboxCount, err)
	}
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), project, item)
		if err == nil && (row.Revision != 2 || hubapi.StringValue(row.WorkflowStateName) != "In Progress") {
			return fmt.Errorf("refused workflow changed state=%s revision=%d", hubapi.StringValue(row.WorkflowStateName), row.Revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("second item has independent workflow receipts", func(t *testing.T) {
		request := transition
		request.IdempotencyKey = "workflow-api"
		response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(blocker)+"/workflow", "operator-secret-token", request)
		requireNativeStatus(t, response, http.StatusOK)
		var second tracker.NativeIssue
		decodeHubResponse(t, response, &second)
		if second.WorkItemID != blocker || second.State != "In Progress" || second.Revision != 2 {
			t.Fatalf("second workflow=%+v", second)
		}
		response = call(t, "operator-secret-token", operatortool.MoveItem, "workflow-api", map[string]any{"identifier": blocker, "target_state": "In Progress", "expected_revision": "1", "workflow_label": "detent:in-progress"})
		requireNativeStatus(t, response, http.StatusOK)
		var envelope struct {
			Result struct {
				Failed  bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		decodeHubResponse(t, response, &envelope)
		var refusal hubapi.NativeError
		if !envelope.Result.Failed || len(envelope.Result.Content) != 1 || json.Unmarshal([]byte(envelope.Result.Content[0].Text), &refusal) != nil || refusal.Code != "invalid_request" || !strings.HasPrefix(refusal.Message, "move_item cannot resume an item already in the requested state") {
			t.Fatalf("second workflow with an HTTP idempotency key=%s, want a distinct MCP retry identity refused", response.Body.String())
		}
		var count, issueNumber int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*),min(issue_number) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, blocker).Scan(&count, &issueNumber); err != nil || count != 1 || issueNumber != 2 {
			t.Fatalf("second workflow outbound count=%d issue=%d error=%v", count, issueNumber, err)
		}
		var held leasestore.Lease
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), project, blocker)
			if err != nil {
				return err
			}
			held, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(worker.binding.MachineID), RunnerID: worker.binding.RunnerID, SessionID: "session_" + string(blocker), LeaseID: "lease_" + string(blocker), TTL: time.Minute})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		cancel := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "cancel-compatible", LeaseID: tracker.LeaseID(held.LeaseID), FencingToken: tracker.FencingToken(held.FencingToken + 1)}, ExpectedRevision: 2, State: "Cancelled", Reason: "user_requested", WorkflowLabel: "detent:cancelled"}
		response = performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(blocker)+"/workflow", "operator-secret-token", cancel)
		requireNativeStatus(t, response, http.StatusConflict)
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			lease, err := r.Lease(t.Context(), held.LeaseID)
			if err == nil && lease.ReleasedAt != nil {
				return fmt.Errorf("stale workflow fence released lease")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		cancel.FencingToken = tracker.FencingToken(held.FencingToken)
		for range 2 {
			response = performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(blocker)+"/workflow", "operator-secret-token", cancel)
			requireNativeStatus(t, response, http.StatusOK)
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			lease, err := r.Lease(t.Context(), held.LeaseID)
			if err != nil {
				return err
			}
			if lease.ReleasedAt == nil || lease.ReleaseReason != "cancelled" {
				return fmt.Errorf("compatible cancellation retained lease: %+v", lease)
			}
			row, err := r.WorkItem(t.Context(), project, blocker)
			if err != nil {
				return err
			}
			if row.Revision != 3 || hubapi.StringValue(row.WorkflowStateName) != "Cancelled" {
				return fmt.Errorf("compatible cancellation state=%s revision=%d", hubapi.StringValue(row.WorkflowStateName), row.Revision)
			}
			entry, err := r.QueueEntry(t.Context(), project, blocker, string(project))
			if err == nil && (entry.State != "Cancelled" || entry.Priority == nil || *entry.Priority != tracker.QueuePriorityUrgent) {
				return fmt.Errorf("compatible cancellation lost queue state/priority: %+v", entry)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, blocker).Scan(&count); err != nil || count != 2 {
			t.Fatalf("compatible cancellation outbound=%d error=%v", count, err)
		}
	})

	t.Run("change workflow derives the approved mapped label", func(t *testing.T) {
		review := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "compatible-review"}, ExpectedRevision: 2, State: "Human Review", Reason: "user_requested", WorkflowLabel: "detent:review"}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/workflow", "operator-secret-token", review), http.StatusOK)
		response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(item)+"/changes", "operator-secret-token", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "compatible-change"}, Title: "Compatible workflow change", Body: "Current change discussion"})
		requireNativeStatus(t, response, http.StatusOK)
		var change tracker.ChangeRequest
		decodeHubResponse(t, response, &change)
		credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken("operator-secret-token"), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("authenticate change operator: status=%d error=%v", status, err)
		}
		scope := nativeScope{organization: organization, project: project, credential: credential}
		plan, err := service.prepareChangeWorkflowTransition(t.Context(), scope, change, ChangeVersionApproval{Policy: descriptor}, false, service.config.now().UTC())
		if err != nil || plan == nil || !plan.eligible {
			t.Fatalf("prepare compatible change transition: plan=%+v error=%v", plan, err)
		}
		if plan.request.State != "Merging" || plan.request.WorkflowLabel != "detent:merging" || plan.outbound == nil {
			t.Fatalf("change workflow omitted approved target label: %+v", plan.request)
		}
		selected := plan.plan.issue
		var stop string
		if _, _, err := service.validateTenantTransitionIssueContract(t.Context(), scope, &selected, &plan.plan.project, "Human Review", plan.request, &stop, nil); err != nil {
			t.Fatalf("nil optional workflow label output: %v", err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			ready, err := service.lockChangeWorkflowTransition(t.Context(), w, scope, plan)
			if err != nil {
				return err
			}
			if !ready {
				return fmt.Errorf("compatible change workflow not ready")
			}
			return service.applyChangeWorkflowTransition(t.Context(), w, scope, plan)
		}); err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.WorkItem(t.Context(), project, item)
			if err == nil && (row.Revision != 4 || hubapi.StringValue(row.WorkflowStateName) != "Merging") {
				return fmt.Errorf("change workflow state=%s revision=%d", hubapi.StringValue(row.WorkflowStateName), row.Revision)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var outbound projectstore.GitHubOutbound
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			outbound, err = w.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), plan.outbound.Key)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		encoded, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), outbound.Payload)
		var desired WorkflowLabelDesired
		if err != nil || json.Unmarshal(encoded, &desired) != nil || desired.ManagedPrefix != "detent:" || desired.Label != "detent:merging" || desired.Clear || outbound.Repository != "example-org/example-project" || outbound.IssueNumber != 1 {
			t.Fatalf("change workflow persisted label=%s target=%+v error=%v", encoded, outbound, err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := service.lockChangeWorkflowTransition(t.Context(), w, scope, plan)
			return err
		}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale change workflow plan error=%v", err)
		}
		var count int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, item).Scan(&count); err != nil || count != 4 {
			t.Fatalf("change workflow outbound count=%d error=%v", count, err)
		}
	})
	for index, test := range []struct {
		name, repository, target string
		disabled, mapped, rework bool
	}{
		{"disabled integration", "example-org/example-project", "Merging", true, true, false},
		{"missing source", "", "In Progress", false, true, true},
		{"foreign source", "example-org/other-project", "Merging", false, true, false},
		{"unmapped automated target", "example-org/example-project", "Merging", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_automated_%d", index))
			retained := newCoreNeutralFixture(t, organization, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
			retained.add("workflow_states", map[string]any{"id": 2, "project_id": project, "detent_state": "Human Review"})
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: id, Number: index + 3, Revision: 1, Profile: "github_compatible"}, Title: "Retained automated transition", Body: "Canonical local review content", State: "Human Review", Actor: actor, CreatedAt: parseTestTime(t, stamp), UpdatedAt: parseTestTime(t, stamp), LastActivityAt: parseTestTime(t, stamp)}
			row := map[string]any{"id": 100 + index, "native_id": id, "project_id": project, "number": index + 3, "revision": 1, "event_sequence": 1, "title": issue.Title, "body": issue.Body, "actor_json": string(historyTestJSON(t, actor)), "archived": 0, "created_at": stamp, "updated_at": stamp, "native_created_at": stamp, "native_updated_at": stamp, "last_activity_at": stamp, "workflow_state_id": 2, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-owner"}
			if test.repository != "" {
				owner, name, ok := strings.Cut(test.repository, "/")
				if !ok {
					t.Fatal("fixture repository is invalid")
				}
				retained.add("repositories", map[string]any{"id": 1, "github_owner": owner, "github_name": name})
				row["repository_id"] = 1
				issue.ExternalReferences = []tracker.ExternalReference{tracker.GitHubIssueSourceReference("I_"+string(id), fmt.Sprintf("https://github.com/%s/issues/%d", test.repository, issue.Number))}
			}
			retained.add("issues", row)
			retained.add("collaboration_versions", map[string]any{"project_id": project, "work_item_id": id, "record_id": id, "revision": 1, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
			retained.add("collaboration_events", map[string]any{"id": "event_" + string(id), "project_id": project, "work_item_id": id, "sequence": 1, "type": "issue.created", "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: 1})), "event_revision": 1, "recorded_at": stamp})
			count, err := ImportCoreWorkItems(t.Context(), service.config.TenantRepository.(*postgres.Repository), CoreWorkItemImportSource{BundlePath: retained.write(), OrganizationID: organization, ProjectID: project, Objects: service.runDetailStore()})
			if err != nil || count != 1 {
				t.Fatalf("retained automated source import=%d error=%v", count, err)
			}
			selectedPolicy := descriptor
			if !test.mapped {
				selectedPolicy.Configuration = &policy.Configuration{Behavior: json.RawMessage(`{"StateMap":{"IsMap":true,"Map":{"In Progress":"detent:in-progress","Done":"detent:done","Cancelled":"detent:cancelled","Human Review":"detent:review"}}}`), Prompt: "Follow the approved workflow"}
				selectedPolicy = selectedPolicy.WithID()
			}
			revision, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, selectedPolicy)
			if err != nil {
				t.Fatal(err)
			}
			revision.ProjectID, revision.ApprovedBy, revision.ApprovedAt = project, "credential_owner", service.config.now()
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				current, err := w.ProjectPolicy(t.Context(), project)
				if err != nil {
					return err
				}
				if err := w.ApproveProjectPolicy(t.Context(), revision, current.PolicyID); err != nil {
					return err
				}
				record, err := w.ProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				record.Settings.RepositoryEnabled = !test.disabled
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(id)+"/changes", "operator-secret-token", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change_" + string(id)}, Title: "Recorded local review", Body: "Keep canonical change history"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			credential, status, err := service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken("operator-secret-token"), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("authenticate automated transition operator: status=%d error=%v", status, err)
			}
			scope := nativeScope{organization: organization, project: project, credential: credential}
			plan, err := service.prepareChangeWorkflowTransition(t.Context(), scope, change, ChangeVersionApproval{Policy: selectedPolicy}, test.rework, service.config.now().UTC())
			if err != nil || plan == nil || !plan.eligible || plan.outbound != nil {
				t.Fatalf("local automated transition refused or prepared external write: plan=%+v error=%v", plan, err)
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				ready, err := service.lockChangeWorkflowTransition(t.Context(), w, scope, plan)
				if err != nil {
					return err
				}
				if !ready {
					return fmt.Errorf("local automated transition not ready")
				}
				return service.applyChangeWorkflowTransition(t.Context(), w, scope, plan)
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.WorkItem(t.Context(), project, id)
				if err != nil {
					return err
				}
				if stored.Revision != 2 || hubapi.StringValue(stored.WorkflowStateName) != test.target {
					return fmt.Errorf("local automated state=%s revision=%d", hubapi.StringValue(stored.WorkflowStateName), stored.Revision)
				}
				dispatch, err := r.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), project, id)
				if err == nil && dispatch.Generation != 1 {
					return fmt.Errorf("local automated dispatch generation=%d", dispatch.Generation)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", organization, project, id).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unavailable external label produced outbound=%d error=%v", count, err)
			}
		})
	}
}

func TestListenerSecurityRequiresLoopbackTLSOrTrustedProxy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "IPv4 loopback", config: Config{ListenAddress: "127.0.0.1:7777"}},
		{name: "IPv6 loopback", config: Config{ListenAddress: "[::1]:7777"}},
		{name: "localhost", config: Config{ListenAddress: "localhost:7777"}},
		{name: "wildcard rejected", config: Config{ListenAddress: "0.0.0.0:7777"}, wantErr: true},
		{name: "public TLS", config: Config{ListenAddress: "0.0.0.0:7777", TLSCertFile: "server.crt", TLSKeyFile: "server.key"}},
		{name: "trusted proxy", config: Config{ListenAddress: "0.0.0.0:7777", TrustedProxy: true}},
		{name: "incomplete TLS", config: Config{ListenAddress: "127.0.0.1:7777", TLSCertFile: "server.crt"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateListenerSecurity(test.config)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateListenerSecurity() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestControlPlaneRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()

	f := newDefaultNativeFixture(t, Config{})
	service := f.service
	service.echo.GET("/health", service.health, service.requireAPIScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator, hubapi.APIScopeAdmin))
	item := f.create(t, "Authentication boundary")
	path := f.base + "/work-items/" + string(item.WorkItemID)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/health"},
		{http.MethodGet, f.base + "/work-items"},
		{http.MethodGet, path},
		{http.MethodPost, f.base + "/claims"},
		{http.MethodPost, f.base + "/leases/lease/renew"},
		{http.MethodPost, f.base + "/leases/lease/release"},
		{http.MethodPost, path + "/events"},
		{http.MethodPost, path + "/workflow"},
		{http.MethodPost, path + "/dependencies"},
		{http.MethodPatch, path},
		{http.MethodPost, f.base + "/machines/register"},
		{http.MethodPost, f.base + "/machines/machine/heartbeat"},
		{http.MethodGet, path + "/history"},
		{http.MethodGet, path + "/runtime"},
		{http.MethodPost, "/api/v2/tokens/token/grants"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			response := performHubAPIRequest(t, service, route.method, route.path, "", nil)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d body = %s, want 401", response.Code, response.Body.String())
			}
		})
	}
	t.Run("private credential maintenance", func(t *testing.T) {
		config := hostedPostgresConfig(t, Config{ListenAddress: "127.0.0.1:0", CredentialMaintenance: true, Hosted: &HostedConfig{OrganizationID: "org_api_auth", WorkOSOrganizationID: "provider_api_auth", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}})
		maintenance := openTestService(t, config)
		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/tokens"},
			{http.MethodPost, "/api/v1/tokens/token/rotate"},
			{http.MethodDelete, "/api/v1/tokens/token"},
		} {
			response := performHubAPIRequest(t, maintenance, route.method, route.path, "", nil)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status=%d body=%s want401", route.method, route.path, response.Code, response.Body.String())
			}
		}
	})
}

func performHubAPIRequest(t *testing.T, service *Service, method string, path string, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	request := newHubAPIRequest(t, method, path, token, payload)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func newHubAPIRequest(t *testing.T, method, path, token string, payload any) *http.Request {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response status %d body %s: %v", response.Code, response.Body.String(), err)
	}
}

func seedHubAPIToken(t *testing.T, service *Service, id string, token string, scope hubapi.APIScope) {
	t.Helper()
	hash := apikey.HashToken(token)
	now := service.config.now().UTC().Truncate(time.Microsecond)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: service.accessOrganization(), ID: id, Name: id, Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: string(scope), Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatalf("insert Hub API token: %v", err)
	}
}
