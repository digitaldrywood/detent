package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func updateWorkflowPolicyFixtureProject(t *testing.T, f nativeFixture, update func(*projectstore.Record)) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		update(&project)
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func saveWorkflowPolicyFixtureGuidance(t *testing.T, f nativeFixture, markdown string) {
	t.Helper()
	raw, err := json.Marshal(markdown)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, raw)
	if err != nil {
		t.Fatal(err)
	}
	updateWorkflowPolicyFixtureProject(t, f, func(project *projectstore.Record) {
		project.Settings.WorkflowKey, project.Settings.WorkflowSHA256, project.Settings.WorkflowBytes, project.Settings.WorkflowContentBytes = ref.PayloadObjectKey, ref.PayloadSHA256, ref.PayloadBytes, ref.PayloadContentBytes
	})
}

func unenrolledWorkflowPolicyFixtureCredential(t *testing.T, f nativeFixture) string {
	t.Helper()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	id := hubapi.NewID("credential")
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	expires := now.Add(runnerauth.CredentialTTL)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: id, Name: "Unenrolled example reporter", Hash: apikey.HashToken(token), Fingerprint: hubapi.TokenFingerprint(apikey.HashToken(token)), Scope: "worker", Kind: "service", NativeOnly: true, ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: id, ProjectID: f.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func installWorkflowPolicyWorkCounts(t *testing.T, f nativeFixture) {
	t.Helper()
	admin := conversationTestAdmin(t, f.service)
	for _, statement := range []string{
		"CREATE TABLE policy_work_counts(organization_id text NOT NULL,project_id text NOT NULL,kind text NOT NULL)",
		"GRANT INSERT ON policy_work_counts TO detent_runtime_v2",
		"CREATE FUNCTION public.count_workflow_policy_fixture() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $count$ BEGIN INSERT INTO public.policy_work_counts VALUES(TG_ARGV[0],TG_ARGV[1],TG_ARGV[2]); RETURN NEW; END $count$",
		"GRANT EXECUTE ON FUNCTION public.count_workflow_policy_fixture() TO detent_runtime_v2",
	} {
		if _, err := admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	organization, project := pgtransport.QuoteLiteral(string(f.project.OrganizationID)), pgtransport.QuoteLiteral(string(f.project.ID))
	for _, target := range []struct{ table, key, operation, kind string }{{"native_board_dirty", "project_id", "INSERT", "card"}, {"project_workflow_states", "project_id", "UPDATE", "state"}, {"projects", "id", "UPDATE", "project"}} {
		statement := "CREATE TRIGGER count_policy_" + target.kind + " AFTER " + target.operation + " ON " + target.table + " FOR EACH ROW WHEN (NEW.organization_id=" + organization + " AND NEW." + target.key + "=" + project + ") EXECUTE FUNCTION public.count_workflow_policy_fixture(" + organization + "," + project + "," + pgtransport.QuoteLiteral(target.kind) + ")"
		if _, err := admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func seedWorkflowPolicyNativeLanding(t *testing.T, f nativeFixture, commit, baseRef string, partial bool) string {
	t.Helper()
	source := f.createIndependent(t, "Landing provenance source")
	approval, err := f.service.readTenantProjectPolicy(t.Context(), f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	runner.enroll(t)
	runner.reportCapacity(t)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, tracker.NativeClaim{WorkItemID: source.WorkItemID, PolicyID: approval.Policy.ID, MachineID: runner.binding.MachineID, SessionID: "landing-source", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	event := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(source.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(runner.redemption.Credential), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("landing actor status%d: %v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	actor := scope.Actor()
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	change := tracker.ChangeRequest{ID: "change_landed", OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: source.WorkItemID, Title: "Landed source", Revision: 1, CreatedAt: now, UpdatedAt: now}
	content := eventstore.NewContent(f.service.runDetailStore())
	changeRow, err := content.PrepareChange(t.Context(), f.project.OrganizationID, historyTestJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	input := changeTestInput()
	input.Repository = "https://github.com/example-org/example-project"
	input.HeadSHA = strings.Repeat("b", 40)
	input.PolicyID = approval.Policy.ID
	input.RunID, input.AttemptID = event.Data.RunID, event.Data.AttemptID
	version := tracker.ChangeVersion{ID: "version_landed", ChangeID: change.ID, Number: 1, ChangeVersionInput: input, Policy: approval.Policy, Actor: analyticsTestScope(t, f).Actor(), CreatedAt: now}
	versionRow, err := content.PrepareVersion(t.Context(), f.project.OrganizationID, historyTestJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	change.CurrentVersion = version.ID
	change.Revision++
	change.Landed = &tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: commit, BaseRef: baseRef, Method: "merge", Actor: actor, LandedAt: now}
	updated, err := content.PrepareChange(t.Context(), f.project.OrganizationID, historyTestJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	updated.Revision = 1
	frozen, err := content.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: source.WorkItemID, ChangeID: change.ID, VersionID: version.ID}, *change.Landed)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := content.PrepareLandingReceipt(t.Context(), eventstore.LandingReceipt{OrganizationID: f.project.OrganizationID, AttemptID: event.Data.AttemptID, VersionID: version.ID, Repository: version.Repository, BaseRef: baseRef}, tracker.NativeLandingReceipt{ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: commit, BaseRef: baseRef, Method: "merge", Landed: true, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: source.WorkItemID, ChangeID: change.ID, CreatedAt: now}); err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
			return err
		}
		if _, err := w.UpdateChange(t.Context(), updated); err != nil {
			return err
		}
		if _, err := w.CreateLandingReceipt(t.Context(), receipt); err != nil {
			return err
		}
		return w.RecordVersionLanding(t.Context(), frozen)
	}); err != nil {
		t.Fatal(err)
	}
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish-landing-source", 2, "succeeded"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, event), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", runner.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	if partial {
		result, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE change_requests SET landed_at=NULL,landed_head_sha=NULL,landed_merge_sha=NULL,landed_base_ref=NULL WHERE organization_id=$1 AND project_id=$2 AND id=$3 AND landed_version_id=$4", f.project.OrganizationID, f.project.ID, change.ID, version.ID)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatalf("partial landing fixture rows%d: %v", count, err)
		}
	}
	return actor.PrincipalID
}

func seedWorkflowPolicyPullRequest(t *testing.T, f nativeFixture, commit, baseRef string, bound, merged bool) {
	t.Helper()
	if bound {
		updateWorkflowPolicyFixtureProject(t, f, func(project *projectstore.Record) { project.Settings.Repository = "example-org/example-project" })
	}
	var mergedAt any
	if merged {
		mergedAt = testTimestamp
	}
	raw, err := json.Marshal(map[string]any{"action": "closed", "repository": map[string]any{"full_name": "example-org/example-project", "default_branch": "develop"}, "pull_request": map[string]any{"node_id": "PR_policy_landing", "number": 1, "title": "Landed workflow instructions", "state": "closed", "draft": false, "html_url": "https://github.com/example-org/example-project/pull/1", "created_at": testTimestamp, "updated_at": testTimestamp, "merge_commit_sha": commit, "merged_at": mergedAt, "merged_by": map[string]any{"login": "landing-owner"}, "base": map[string]any{"ref": baseRef, "sha": strings.Repeat("a", 40)}, "head": map[string]any{"ref": "workflow-source", "sha": commit}}})
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.service.runDetailStore())
	ref, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, raw)
	if err != nil {
		t.Fatal(err)
	}
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	row := projectstore.GitHubWebhook{DeliveryID: "policy-landing", Repository: "example-org/example-project", EventType: "pull_request", Action: "closed", Digest: policy.Digest(raw), Payload: ref, ReceivedAt: now, ExpiresAt: now.Add(time.Hour), NextAttemptAt: now, Status: "pending"}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		created, err := w.(projectstore.GitHubIntakeWriter).CreateGitHubWebhook(t.Context(), row)
		if err == nil && !created {
			return errors.New("landing webhook fixture already exists")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.maintainGitHubWebhookRecords(t.Context()); err != nil {
		t.Fatal(err)
	}
	var stored projectstore.GitHubWebhook
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		stored, err = r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), row.DeliveryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	persisted, err := content.Receipt(t.Context(), stored.Payload)
	if err != nil || string(persisted) != string(raw) || stored.Status != "completed" {
		t.Fatalf("landing webhook source was not retained/completed: status=%s err=%v", stored.Status, err)
	}
}

func TestObservedRepositoryWorkflowApply(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                                                                                  string
		initial, reachable, local, occupied, wrongRepository, legacy, missingRevision, replay bool
		status                                                                                int
		applied                                                                               bool
		canonical, changedInputs, active                                                      bool
		fromLegacy                                                                            bool
		forge                                                                                 string
		nilConfiguration                                                                      bool
		humanApproval                                                                         bool
		fleet                                                                                 bool
		autoInitial                                                                           bool
		bind, adopt, nativeRevision                                                           bool
		history                                                                               int
		historyLegacy                                                                         string
		landing                                                                               string
		unbound, unmerged, otherBranch, lateLanding, liesDefaultBranch                        bool
		partialLanding, otherCommit                                                           bool
		largeHistory                                                                          bool
		changedSource, concurrentReports                                                      bool
	}{
		{name: "native landing applies instructions without reachability report", initial: true, canonical: true, changedInputs: true, landing: "native", active: true, status: http.StatusNoContent, applied: true},
		{name: "bound merged PR applies instructions without reachability report", initial: true, canonical: true, changedInputs: true, landing: "pr", active: true, status: http.StatusNoContent, applied: true},
		{name: "runner cannot authorize feature PR by calling it default", initial: true, canonical: true, changedInputs: true, landing: "pr", otherBranch: true, liesDefaultBranch: true, status: http.StatusNoContent},
		{name: "native landing recorded after default branch apply gains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "native", lateLanding: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "merged PR recorded after default branch apply gains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "pr", lateLanding: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "native landing recorded after unlanded observation stays pending", initial: true, canonical: true, changedInputs: true, landing: "native", lateLanding: true, status: http.StatusNoContent},
		{name: "merged PR recorded after unlanded observation stays pending", initial: true, canonical: true, changedInputs: true, landing: "pr", lateLanding: true, status: http.StatusNoContent},
		{name: "native default branch landing retains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "native", status: http.StatusNoContent, applied: true},
		{name: "merged PR default branch landing retains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "pr", status: http.StatusNoContent, applied: true},
		{name: "native landing on another branch stays pending", initial: true, canonical: true, changedInputs: true, landing: "native", otherBranch: true, status: http.StatusNoContent},
		{name: "partially backfilled native landing applies matching source", initial: true, canonical: true, changedInputs: true, landing: "native", partialLanding: true, status: http.StatusNoContent, applied: true},
		{name: "partially backfilled native landing on another branch stays pending", initial: true, canonical: true, changedInputs: true, landing: "native", partialLanding: true, otherBranch: true, status: http.StatusNoContent},
		{name: "partially backfilled native landing of another commit stays pending", initial: true, canonical: true, changedInputs: true, landing: "native", partialLanding: true, otherCommit: true, status: http.StatusNoContent},
		{name: "merged PR on another branch stays pending", initial: true, canonical: true, changedInputs: true, landing: "pr", otherBranch: true, status: http.StatusNoContent},
		{name: "unbound merged PR stays pending", initial: true, canonical: true, changedInputs: true, landing: "pr", unbound: true, status: http.StatusNoContent},
		{name: "closed unmerged PR stays pending", initial: true, canonical: true, changedInputs: true, landing: "pr", unmerged: true, status: http.StatusNoContent},
		{name: "runner current policy with retained authored history", initial: true, autoInitial: true, canonical: true, reachable: true, replay: true, largeHistory: true, status: http.StatusNoContent, applied: true},
		{name: "instruction change with retained history", initial: true, autoInitial: true, canonical: true, changedInputs: true, reachable: true, active: true, history: 64, status: http.StatusNoContent, applied: true},
		{name: "instruction change with legacy policy identity", initial: true, autoInitial: true, canonical: true, changedInputs: true, reachable: true, active: true, history: 2, historyLegacy: "policy", status: http.StatusNoContent, applied: true},
		{name: "instruction change with legacy review identity", initial: true, autoInitial: true, canonical: true, changedInputs: true, reachable: true, active: true, history: 2, historyLegacy: "review", status: http.StatusNoContent, applied: true},
		{name: "binding applies reported repository configuration", initial: true, bind: true, canonical: true, changedInputs: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "binding without approval applies first repository policy", bind: true, canonical: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "binding carries same authored approval", initial: true, bind: true, canonical: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "explicit repository adoption enables later updates", initial: true, bind: true, adopt: true, canonical: true, changedInputs: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "repository configuration replaces unversioned native definition", initial: true, nativeRevision: true, canonical: true, changedInputs: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "external authored definition stays pending against legacy approval", initial: true, canonical: true, fromLegacy: true, local: true, active: true, status: http.StatusNoContent},
		{name: "default branch replaces legacy approval", initial: true, canonical: true, fromLegacy: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "nil configuration projection carries only authored policy", initial: true, canonical: true, active: true, nilConfiguration: true, forge: "gate", status: http.StatusNoContent, applied: true},
		{name: "nil configuration invented digest is refused", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "digest", status: http.StatusUnprocessableEntity},
		{name: "canonical version carries feature approval", initial: true, canonical: true, status: http.StatusNoContent, applied: true},
		{name: "canonical version preserves active lease", initial: true, canonical: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "canonical version without provenance carries approval", initial: true, canonical: true, local: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "canonical version with native landing preserves approver", initial: true, canonical: true, landing: "native", status: http.StatusNoContent, applied: true},
		{name: "canonical version with merged PR preserves approver", initial: true, canonical: true, landing: "pr", status: http.StatusNoContent, applied: true},
		{name: "canonical version wrong repository carries approval", initial: true, canonical: true, wrongRepository: true, status: http.StatusNoContent, applied: true},
		{name: "changed authored workflow without provenance stays pending", initial: true, canonical: true, local: true, changedInputs: true, status: http.StatusNoContent},
		{name: "changed workflow source without provenance stays pending", initial: true, canonical: true, local: true, changedSource: true, status: http.StatusNoContent},
		{name: "concurrent different authored reports stay conflicting", initial: true, canonical: true, local: true, changedInputs: true, concurrentReports: true, status: http.StatusNoContent},
		{name: "copied provenance cannot change gate", initial: true, canonical: true, local: true, active: true, forge: "gate", status: http.StatusNoContent, applied: true},
		{name: "copied provenance cannot change selector", initial: true, canonical: true, local: true, active: true, forge: "selector", status: http.StatusNoContent, applied: true},
		{name: "copied provenance cannot change behavior", initial: true, canonical: true, local: true, active: true, forge: "behavior", status: http.StatusNoContent, applied: true},
		{name: "copied provenance cannot change local grant", initial: true, canonical: true, local: true, active: true, forge: "grant", status: http.StatusUnprocessableEntity},
		{name: "nil configuration cannot change gate", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "gate", status: http.StatusNoContent, applied: true},
		{name: "nil configuration cannot change selector", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "selector", status: http.StatusNoContent, applied: true},
		{name: "nil configuration cannot change profile", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "profile", status: http.StatusNoContent, applied: true},
		{name: "canonical version with changed inputs pending", initial: true, canonical: true, changedInputs: true, status: http.StatusNoContent},
		{name: "default branch changed authored files auto apply", initial: true, canonical: true, changedInputs: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "default branch initial definition", reachable: true, status: http.StatusNoContent, applied: true},
		{name: "second default branch revision auto applies", initial: true, autoInitial: true, reachable: true, active: true, fleet: true, status: http.StatusNoContent, applied: true},
		{name: "different tiers share authored approval", initial: true, canonical: true, active: true, fleet: true, status: http.StatusNoContent, applied: true},
		{name: "human approval records each apply", initial: true, replay: true, active: true, fleet: true, status: http.StatusNoContent, applied: true, humanApproval: true},
		{name: "feature branch pending", initial: true, status: http.StatusNoContent},
		{name: "uncommitted definition pending", initial: true, local: true, reachable: true, status: http.StatusNoContent},
		{name: "occupied lane stays pending", initial: true, reachable: true, occupied: true, status: http.StatusConflict},
		{name: "different repository pending", initial: true, reachable: true, wrongRepository: true, status: http.StatusNoContent},
		{name: "unenrolled reporter pending", initial: true, reachable: true, legacy: true, status: http.StatusNoContent},
		{name: "missing commit pending", initial: true, reachable: true, missingRevision: true, status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", test.name)
			clearNativeFixturePolicy(t, f)
			if !test.bind {
				updateWorkflowPolicyFixtureProject(t, f, func(project *projectstore.Record) {
					project.Settings.CheckoutRepository = "example-org/example-project"
				})
			}
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			runner.enroll(t)
			runner.reportCapacity(t)
			var canonicalPolicy policy.Descriptor
			original := hubTestPolicy()
			original.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("a", 40), States: nativeFixtureStates()}
			if test.canonical {
				instructions := "Run the work.\n"
				if test.largeHistory {
					instructions = strings.Repeat(instructions, 14000)
				}
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte(instructions), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Done: [Todo]\n"), HasConfig: true, ConfigPath: "detent.yaml"})
				if err != nil {
					t.Fatal(err)
				}
				workflow.Definition.Revision = strings.Repeat("a", 40)
				canonicalPolicy, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				if test.fromLegacy {
					workflow.Config.Gate.Run = "approved-validator --fast"
					workflow.Config.Gate.Validator.Enabled = true
					workflow.Config.Worker.AllowLocalBinding = new(true)
					workflow.Config.Worker.ExtraNetworkDomains = []string{"private.example.test"}
					workflow.Authored = nil
					workflow.DefinitionSources = nil
				} else {
					workflow.Authored.Version = 1
				}
				original, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.nativeRevision {
				workflow := *original.Workflow
				workflow.Revision = ""
				original.Workflow = &workflow
			}
			original = original.WithID()
			historyCount := 0
			var initialApproval policy.Approval
			if test.initial {
				if test.autoInitial {
					initialSource := &policy.RepositorySource{Repository: "example-org/example-project", Commit: original.Workflow.Revision, DefaultBranch: "develop", DefaultBranchHead: original.Workflow.Revision, DefaultBranchReachable: true}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", runner.redemption.Credential, policy.Observation{Descriptor: original, Source: initialSource}), http.StatusNoContent)
				} else {
					approveHubTestPolicy(t, f.service, f.base+"/policy", original)
				}
				historyCount++
				var err error
				initialApproval, _, err = f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.bind {
				f.service.config.ReconcileBackend = &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_example", Owner: "example-org", Name: "example-project", UpdatedAt: time.Now().UTC()}}}}}
				project, err := f.service.readTenantProjectRecord(t.Context(), analyticsTestScope(t, f))
				integration := projectIntegrationRecord(project)
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/repository", testHubAdminToken, bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "bind-repository"}, ExpectedRevision: integration.Revision, Repository: "example-org/example-project"}), http.StatusOK)
			}
			if !test.initial {
				seedNativeFixturePolicy(t, f.service, string(f.project.OrganizationID)+"/"+string(f.project.ID))
			}
			item := f.create(t, "Preserve occupied Todo and revision")
			if !test.initial {
				clearNativeFixturePolicy(t, f)
			}
			baseline := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
			requireNativeStatus(t, baseline, http.StatusOK)
			decodeHubResponse(t, baseline, &item)
			candidate := original
			candidate.SourceDigest = policy.Digest([]byte("changed repository definition"))
			candidate.SourceRevision = strings.Repeat("b", 40)
			candidate.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("b", 40), States: append(nativeFixtureStates(), policy.State{Name: "Repair", Dispatchable: true})}
			if test.occupied {
				candidate.Workflow.States = []policy.State{{Name: "Repair", Dispatchable: true}}
			}
			if test.missingRevision {
				candidate.Workflow.Revision = ""
			}
			if test.canonical {
				candidate = canonicalPolicy
				authored := *canonicalPolicy.Authored
				if test.changedInputs {
					files := make(map[string]string)
					for name, content := range authored.Files {
						files[name] = content
					}
					files["WORKFLOW.md"] = "Changed authored instructions.\n"
					workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte(files["WORKFLOW.md"]), Config: []byte(files["detent.yaml"]), HasConfig: true})
					if err != nil {
						t.Fatal(err)
					}
					updated, err := workflowconfig.ResolvePolicy(workflow)
					if err != nil {
						t.Fatal(err)
					}
					authored = *updated.Authored
				}
				candidate.Authored = &authored
				candidate.SourceDigest, candidate.ConfigDigest, candidate.SourceRevision = authored.Digest, authored.Digest, authored.Digest
				workflow := *canonicalPolicy.Workflow
				workflow.Revision = strings.Repeat("b", 40)
				candidate.Workflow = &workflow
			}
			switch test.forge {
			case "digest":
				candidate.Authored.Digest = policy.Digest([]byte("invented identity"))
				candidate.SourceDigest, candidate.ConfigDigest, candidate.SourceRevision = candidate.Authored.Digest, candidate.Authored.Digest, candidate.Authored.Digest
				candidate.Gates.AutoPromote = !candidate.Gates.AutoPromote
			case "gate":
				candidate.Gates.AutoPromote = !candidate.Gates.AutoPromote
			case "profile":
				candidate.Profile = "privileged"
			case "selector":
				candidate.Requirements = policy.Requirements{MachineID: "machine_privileged"}
			case "behavior":
				configuration := policy.Configuration{}
				configuration.Behavior = []byte(`{"Worker":{"AllowLocalBinding":true}}`)
				candidate.Configuration = &configuration
			case "grant":
				files := make(map[string]string)
				for name, content := range candidate.Authored.Files {
					files[name] = content
				}
				files["detent.local.yaml"] = "schema: 1\nworker:\n  allow_local_binding: true\n"
				candidate.Authored.Files = files
			}
			if test.nilConfiguration {
				candidate.Configuration = nil
			}
			if test.changedSource {
				candidate.Workflow.Source = "other.yaml"
			}
			candidate = candidate.WithID()
			source := &policy.RepositorySource{Repository: "example-org/example-project", Commit: strings.Repeat("b", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("c", 40), DefaultBranchReachable: test.reachable}
			if test.liesDefaultBranch {
				source.DefaultBranch = "feature"
			}
			if test.local {
				source = nil
			}
			if test.wrongRepository {
				source.Repository = "example-org/other-project"
			}
			landingActor := "landing-owner"
			if test.landing == "native" {
				landingActor = "runner_landing"
			}
			seedLanding := func() {
				if test.landing == "" {
					return
				}
				baseRef := "develop"
				if test.otherBranch {
					baseRef = "feature"
				}
				if test.landing == "native" {
					commit := source.Commit
					if test.otherCommit {
						commit = strings.Repeat("e", 40)
					}
					landingActor = seedWorkflowPolicyNativeLanding(t, f, commit, baseRef, test.partialLanding)
				} else {
					seedWorkflowPolicyPullRequest(t, f, source.Commit, baseRef, !test.unbound, !test.unmerged)
				}
			}
			if !test.lateLanding {
				seedLanding()
			}
			token := runner.redemption.Credential
			if test.legacy {
				token = unenrolledWorkflowPolicyFixtureCredential(t, f)
			}
			var pinned tracker.NativeLease
			worker := ""
			if test.active {
				active := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				if !test.fleet {
					active.redemption.Capacity = 1
				}
				active.enroll(t)
				worker = active.redemption.Credential
				machine := active.binding.MachineID
				if test.fleet {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, active.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "Active runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}, "isolation_tier": isolation.NativeTrusted}), http.StatusOK)
					if test.humanApproval {
						candidate.Requirements.MachineID = string(runner.binding.MachineID)
						candidate = candidate.WithID()
					}
				}
				active.reportCapacity(t)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: item.WorkItemID, PolicyID: original.ID, MachineID: machine, SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &pinned)
				if test.fleet && pinned.IsolationPolicy.Tier != isolation.NativeTrusted {
					t.Fatalf("first runner tier = %s", pinned.IsolationPolicy.Tier)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &item)
			}
			historySummaries := map[string]tracker.ChangeSummary{}
			historyItems := map[string]tracker.NativeWorkItemID{}
			if test.history > 0 {
				linked := []tracker.NativeWorkItemID{item.WorkItemID}
				for i := range test.history {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-%d", i)}, Title: "Retained history", Body: issueContractTestSections, State: "Done"})
					requireNativeStatus(t, response, http.StatusOK)
					var historical tracker.NativeIssue
					decodeHubResponse(t, response, &historical)
					linked = append(linked, historical.WorkItemID)
				}
				for i, id := range linked {
					path := f.base + "/work-items/" + string(id) + "/changes"
					response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-change-%d", i)}, Title: "Retained Change"})
					requireNativeStatus(t, response, http.StatusOK)
					var change tracker.ChangeRequest
					decodeHubResponse(t, response, &change)
					path += "/" + change.ID
					input := changeTestInput()
					input.PolicyID = original.ID
					response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-version-%d", i)}, ChangeVersionInput: input})
					requireNativeStatus(t, response, http.StatusOK)
					if i > 0 && test.historyLegacy != "" {
						var version tracker.ChangeVersion
						decodeHubResponse(t, response, &version)
						version.ID, version.Number = "legacy_"+change.ID, 2
						if test.historyLegacy == "policy" {
							version.Policy.ID = ""
						} else {
							version.ReviewPolicy.ID = ""
						}
						raw, err := json.Marshal(version)
						if err != nil {
							t.Fatal(err)
						}
						scope := analyticsTestScope(t, f)
						content := eventstore.NewContent(f.service.runDetailStore())
						versionRow, err := content.PrepareVersion(t.Context(), scope.Organization, raw)
						if err != nil {
							t.Fatal(err)
						}
						current, err := f.service.readChangeRecord(t.Context(), scope, string(id), change.ID)
						if err != nil {
							t.Fatal(err)
						}
						revision := current.Revision
						current.CurrentVersion = version.ID
						current.Revision++
						changeRow, err := content.PrepareChange(t.Context(), scope.Organization, historyTestJSON(t, current))
						if err != nil {
							t.Fatal(err)
						}
						changeRow.Revision = int64(revision)
						if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
							if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
								return err
							}
							_, err := w.UpdateChange(t.Context(), changeRow)
							return err
						}); err != nil {
							t.Fatal(err)
						}

					}
					response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var detail tracker.ChangeDetail
					decodeHubResponse(t, response, &detail)
					historySummaries[path] = detail.Summary
					historyItems[path] = id
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &item)
				installWorkflowPolicyWorkCounts(t, f)
			}
			report := policy.Observation{Descriptor: candidate, Source: source}
			if test.concurrentReports {
				other := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				other.enroll(t)
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte("Other runner instructions.\n"), Config: []byte(candidate.Authored.Files["detent.yaml"]), HasConfig: true, ConfigPath: candidate.Workflow.Source})
				if err != nil {
					t.Fatal(err)
				}
				workflow.Definition.Revision = strings.Repeat("d", 40)
				descriptor, err := workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/policy/observed", other.redemption.Credential, policy.Observation{Descriptor: descriptor})
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					done <- response
				}()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report)
				otherResponse := <-done
				requireNativeStatus(t, response, test.status)
				requireNativeStatus(t, otherResponse, http.StatusNoContent)
			} else {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report)
				requireNativeStatus(t, response, test.status)
				if test.occupied {
					var refused hubapi.NativeError
					decodeHubResponse(t, response, &refused)
					if refused.Code != "workflow_conflict" {
						t.Fatalf("occupied lane refusal = %+v", refused)
					}
				}
			}
			if test.lateLanding {
				seedLanding()
			}
			if test.history > 0 {
				for path, before := range historySummaries {
					response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var detail tracker.ChangeDetail
					decodeHubResponse(t, response, &detail)
					if test.historyLegacy != "" && historyItems[path] != item.WorkItemID {
						if detail.Summary.Status != "stale_policy" {
							t.Fatalf("legacy summary = %#v", detail.Summary)
						}
					} else if !reflect.DeepEqual(detail.Summary, before) {
						t.Errorf("instruction-only adoption changed summary: before=%#v after=%#v", before, detail.Summary)
					}
					drainTestNativeBoard(t, f.service)
					scope := analyticsTestScope(t, f)
					page, err := f.service.readBoardRecords(f.service.runDetailContext(t.Context()), scope, url.Values{"include": {"board"}, "limit": {"2000"}})
					if err != nil {
						t.Fatal(err)
					}
					var card tracker.NativeBoardCard
					for _, candidate := range page.Cards {
						if candidate.Issue.WorkItemID == historyItems[path] {
							card = candidate
							break
						}
					}
					if card.Change == nil || card.Change.Status != detail.Summary.Status {
						t.Fatalf("cached summary=%#v, canonical summary=%#v", card.Change, detail.Summary)
					}
				}
				for _, kind := range []string{"card", "state", "project"} {
					var count int
					if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM policy_work_counts WHERE organization_id=$1 AND project_id=$2 AND kind=$3", f.project.OrganizationID, f.project.ID, kind).Scan(&count); err != nil {
						t.Fatal(err)
					}
					t.Logf("instruction-only adoption: %s work=%d, retained history=%d", kind, count, test.history)
					want := 0

					if kind == "project" {
						want = 1
					}
					if count != want {
						t.Errorf("instruction-only adoption %s work=%d, want %d", kind, count, want)
					}
				}
			}
			if test.adopt {
				approval, _, err := f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
				if err != nil || approval.Policy.ID != candidate.ID {
					t.Fatalf("repository report was not applied: %+v %v", approval, err)
				}
				original = approval.Policy
				historyCount++
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte("Updated adopted repository instructions.\n"), Config: []byte(original.Authored.Files["detent.yaml"]), HasConfig: true, ConfigPath: "detent.yaml"})
				if err != nil {
					t.Fatal(err)
				}
				workflow.Definition.Revision = strings.Repeat("d", 40)
				candidate, err = workflowconfig.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				source.Commit, source.DefaultBranchHead = candidate.Workflow.Revision, candidate.Workflow.Revision
				report = policy.Observation{Descriptor: candidate, Source: source}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
			}
			if test.humanApproval {
				approval, _, err := f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
				if err != nil || approval.Policy.ID != original.ID || strings.HasPrefix(approval.ApprovedBy, "runner_") {
					t.Fatalf("runner replaced administrator approval: %+v %v", approval, err)
				}
				fresh := f.create(t, "Second runner uses reported configuration")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidate.ID, MachineID: runner.binding.MachineID, SessionID: "pending", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: original.ID, Policy: candidate}), http.StatusOK)
			}
			if test.forge != "" && !test.applied {
				approval, _, err := f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
				if err != nil || approval.Policy.ID != original.ID || !reflect.DeepEqual(approval.Policy.Gates, original.Gates) || !reflect.DeepEqual(approval.Policy.Requirements, original.Requirements) || approval.Policy.Profile != original.Profile {
					t.Fatalf("forgery changed approval: %+v %v", approval, err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusOK)
				return
			}
			if test.applied && test.canonical {
				resolved, err := workflowconfig.ResolveSharedPolicy(candidate)
				if err != nil {
					t.Fatal(err)
				}
				candidate = resolved
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			if test.applied {
				historyCount++
				if test.initial && candidate.SameAuthoredInputs(original) && setup.Policy != nil && setup.Policy.ApprovedBy != initialApproval.ApprovedBy {
					t.Fatalf("carryover changed approver: got %s, want %s", setup.Policy.ApprovedBy, initialApproval.ApprovedBy)
				}
				if test.reachable && !candidate.SameAuthoredInputs(original) && setup.Policy != nil && setup.Policy.ApprovedBy != "repository-default-branch" && test.landing == "" {
					t.Fatalf("default branch apply attribution = %s", setup.Policy.ApprovedBy)
				}
				if setup.Policy == nil || setup.Policy.Policy.ID != candidate.ID || len(setup.ObservedPolicies) != 0 {
					t.Fatalf("default-branch apply left pending approval: %+v", setup)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
			} else if test.concurrentReports {
				if setup.Policy == nil || setup.Policy.Policy.ID != original.ID || len(setup.ObservedPolicies) != 2 {
					t.Fatalf("conflicting reports changed approval or lost candidate: %+v", setup)
				}
				for _, observed := range setup.ObservedPolicies {
					if !observed.Conflict || observed.PreviouslyApproved {
						t.Fatalf("different authored report not pending with conflict: %+v", observed)
					}
				}
			} else if len(setup.ObservedPolicies) != 1 || setup.Policy == nil || setup.Policy.Policy.ID != original.ID || !reflect.DeepEqual(setup.ObservedPolicies[0].Source, source) {
				t.Fatalf("unapplied revision changed approval or lost report: %+v", setup)
			}
			if test.bind && test.initial && !test.applied && (!reflect.DeepEqual(*setup.Policy, initialApproval) || setup.ObservedPolicies[0].Policy.ID != candidate.ID || setup.ObservedPolicies[0].PreviouslyApproved) {
				t.Fatalf("binding changed approval provenance or lost pending candidate: %+v", setup)
			}
			if strings.HasPrefix(setup.Policy.ApprovedBy, "runner_") && test.landing == "" {
				t.Fatalf("runner is policy approver: %+v", setup.Policy)
			}
			if test.fleet && !test.applied {
				fresh := f.create(t, "Differing runner remains pending")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidate.ID, MachineID: runner.binding.MachineID, SessionID: "pending", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusOK)
			}
			if test.occupied {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: original.ID, Policy: candidate}), http.StatusUnprocessableEntity)
			}
			if test.applied && test.initial && test.canonical && !test.fromLegacy {
				previousReport := policy.Observation{Descriptor: original}
				if source != nil {
					previousSource := *source
					previousSource.Commit = original.Workflow.Revision
					previousReport.Source = &previousSource
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, previousReport), http.StatusNoContent)
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &setup)
				if setup.Policy == nil || setup.Policy.Policy.ID != candidate.ID {
					t.Fatalf("older runner reverted approval: %+v", setup.Policy)
				}
				if len(setup.ObservedPolicies) != 0 {
					t.Fatalf("historical approved revision requested reapproval: %+v", setup.ObservedPolicies)
				}
			}
			if test.active {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/validate", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken}), http.StatusOK)
				if test.fleet && !test.humanApproval {
					fresh := f.create(t, "First runner dispatch authority")
					claimStatus := http.StatusOK
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: original.ID, MachineID: pinned.MachineID, SessionID: "continued", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), claimStatus)
				}
				var policyID string
				err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					pin, err := r.LeasePolicy(t.Context(), pinned.ID)
					policyID = pin.PolicyID
					return err
				})
				if err != nil || policyID != original.ID {
					t.Fatalf("lease pin changed: %s, %v", policyID, err)
				}
			}
			if test.replay {
				previousSource := *source
				previousSource.Commit = original.Workflow.Revision
				previousReport := policy.Observation{Descriptor: original, Source: &previousSource}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, previousReport), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: candidate.ID, Policy: original}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: original.ID, Policy: candidate}), http.StatusOK)
				historyCount += 2
			}
			var approval policy.Approval
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &approval)
			if len(approval.History) != historyCount {
				t.Fatalf("history count = %d, want %d", len(approval.History), historyCount)
			}
			if test.largeHistory {
				if response.Body.Len() <= 1<<20 {
					t.Fatalf("retained history did not reproduce oversized response: %d bytes", response.Body.Len())
				}
				client, err := hubclient.New(hubclient.Config{URL: "https://policy-history.example.test", TokenSource: func() string { return token }, HTTPClient: &http.Client{Transport: policyAPITransport{service: f.service}}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, err := native.ProjectPolicy(t.Context())
				if err != nil {
					t.Fatalf("runner cannot read current policy with retained history: %v", err)
				}
				if !reflect.DeepEqual(current.Policy, approval.Policy) || current.ApprovedBy != approval.ApprovedBy || current.ApprovedAt != approval.ApprovedAt || len(current.History) != 0 || current.HistoryNext != "" {
					t.Fatal("runner current policy changed approval or included unrelated history")
				}
				currentResponse := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", token, nil)
				requireNativeStatus(t, currentResponse, http.StatusOK)
				t.Logf("human history response bytes=%d, runner current response bytes=%d", response.Body.Len(), currentResponse.Body.Len())
			}
			if test.applied {
				entry := approval.History[0]
				if test.landing != "" {
					wantActor := landingActor
					if test.initial && candidate.SameAuthoredInputs(original) {
						wantActor = initialApproval.ApprovedBy
					} else if test.lateLanding {
						wantActor = "repository-default-branch"
					}
					if entry.AppliedBy != wantActor || approval.ApprovedBy != wantActor || test.landing == "native" && entry.ChangeID != "change_landed" || test.landing == "pr" && entry.PullRequestURL != "https://github.com/example-org/example-project/pull/1" {
						t.Fatalf("missing landing provenance: %+v", approval)
					}
				}
				previous := ""
				if test.initial {
					previous = original.SourceDigest
				}
				repository, commit := "example-org/example-project", candidate.Workflow.Revision
				if test.initial && candidate.SameAuthoredInputs(original) && (test.local || test.wrongRepository) && !test.humanApproval {
					repository, commit = "", ""
				}
				if entry.Repository != repository || entry.Commit != commit || entry.PreviousDefinitionDigest != previous || entry.DefinitionDigest != candidate.SourceDigest || entry.RunnerID != runner.binding.RunnerID || entry.AppliedBy != approval.ApprovedBy || strings.HasPrefix(entry.AppliedBy, "runner_") && test.landing == "" || entry.AppliedAt == "" {
					t.Fatalf("incomplete apply history: %+v", entry)
				}
				if !reflect.DeepEqual(entry.Definition, &candidate) || test.initial && !reflect.DeepEqual(entry.PreviousDefinition, &original) || !test.initial && entry.PreviousDefinition != nil {
					t.Fatalf("history does not retain before/after definitions: %+v", entry)
				}
			}
			if test.applied && test.initial {
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy?limit=1", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var firstPage policy.Approval
				decodeHubResponse(t, response, &firstPage)
				if len(firstPage.History) != 1 || !reflect.DeepEqual(firstPage.History[0], approval.History[0]) || firstPage.HistoryNext == "" {
					t.Fatalf("first history page = %+v", firstPage)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy?limit=1&after="+firstPage.HistoryNext, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var nextPage policy.Approval
				decodeHubResponse(t, response, &nextPage)
				if len(nextPage.History) != 1 || !reflect.DeepEqual(nextPage.History[0], approval.History[1]) {
					t.Fatalf("next history page = %+v", nextPage)
				}
			}
			project, err := f.service.readTenantProjectRecord(t.Context(), analyticsTestScope(t, f))
			wantStates := nativeFixtureStates()
			if test.initial {
				wantStates = original.Workflow.States
			}
			if test.applied {
				wantStates = candidate.Workflow.States
			}
			wantStates = hubrecords.NativeTriageStates(wantStates)
			if err != nil || !reflect.DeepEqual(project.States, wantStates) {
				t.Fatalf("workflow states = %+v, %v; want %+v", project.States, err, wantStates)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var retained = item
			decodeHubResponse(t, response, &retained)
			if retained.ChangeSequence < item.ChangeSequence {
				t.Fatal("apply regressed project change sequence")
			}
			item.ChangeSequence = retained.ChangeSequence
			if !reflect.DeepEqual(retained, item) {
				t.Fatalf("apply remapped or edited work item: %+v -> %+v", item, retained)
			}
			if test.fromLegacy && test.local {
				local, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte(candidate.Authored.Files["WORKFLOW.md"]), Config: []byte(candidate.Authored.Files["detent.yaml"]), HasConfig: true, WorkflowPath: "/operator/private/WORKFLOW.md", ConfigPath: "/operator/private/detent.yaml"})
				if err != nil {
					t.Fatal(err)
				}
				local.Definition.Revision = candidate.Workflow.Revision
				local.Config.Hooks.BeforeRun = "private-isolation.sh"
				client, err := hubclient.New(hubclient.Config{URL: "https://legacy-policy.example.test", TokenSource: func() string { return token }, HTTPClient: &http.Client{Transport: policyAPITransport{service: f.service}}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(f.project.OrganizationID, f.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := native.ResolveProjectWorkflow(t.Context(), local, nil)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := workflowconfig.ResolvePolicy(loaded)
				if err != nil || actual.Match(candidate) != nil || loaded.Config.Hooks != local.Config.Hooks {
					t.Fatalf("legacy approval hid supplied authored candidate: %+v %v", actual, err)
				}
				second := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				second.redemption.Capacity = 1
				second.enroll(t)
				second.reportCapacity(t)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", second.redemption.Credential, report), http.StatusNoContent)
				fresh := f.create(t, "Claim retained legacy approval after upgrade")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", second.redemption.Credential, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidate.ID, MachineID: second.binding.MachineID, SessionID: "unapproved", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusOK)
				current, err := native.ProjectPolicy(t.Context())
				if err != nil || current.Policy.ID != original.ID || current.ApprovedBy != approval.ApprovedBy || current.ApprovedAt != approval.ApprovedAt || len(current.History) != 0 {
					t.Fatalf("legacy load rewrote approval or included history: %+v %v", current, err)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var retained policy.Approval
				decodeHubResponse(t, response, &retained)
				if !reflect.DeepEqual(retained, approval) {
					t.Fatal("legacy runner load changed operator approval or retained history")
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", testHubAdminToken, map[string]string{"expected_policy_id": original.ID}), http.StatusGone)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusOK)
			}
		})
	}
}

func TestObservedCloudWorkflowGuidanceAdoptsSameContract(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "cloud-guidance")
	markdown := "---\ntracker:\n  kind: hub_native\ngate:\n  run: make check-landing\n  security_audit:\n    enabled: false\n---\nExisting guidance.\n"
	resolve := func(content string) policy.Descriptor {
		t.Helper()
		workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(content)})
		if err != nil {
			t.Fatal(err)
		}
		workflow.Definition.Layout = workflowconfig.ProjectDefinitionCloud
		descriptor, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		return descriptor
	}
	saveWorkflowPolicyFixtureGuidance(t, f, markdown)
	before := resolve(markdown)
	approveHubTestPolicy(t, f.service, f.base+"/policy", before)
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	runner.enroll(t)
	for _, test := range []struct {
		name, selected, saved string
		applied               bool
	}{
		{"saved guidance", strings.Replace(markdown, "Existing guidance", "Updated guidance", 1), strings.Replace(markdown, "Existing guidance", "Updated guidance", 1), true},
		{"unsaved guidance", strings.Replace(markdown, "Existing guidance", "Invented guidance", 1), strings.Replace(markdown, "Existing guidance", "Updated guidance", 1), false},
		{"new security contract", strings.Replace(markdown, "enabled: false", "enabled: true", 1), strings.Replace(markdown, "enabled: false", "enabled: true", 1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			saveWorkflowPolicyFixtureGuidance(t, f, test.saved)
			selected := resolve(test.selected)
			prior, _, err := f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", runner.redemption.Credential, policy.Observation{Descriptor: selected})
			want := http.StatusNoContent
			if test.name == "unsaved guidance" {
				want = http.StatusConflict
			}
			requireNativeStatus(t, response, want)
			after, _, err := f.service.readTenantProjectPolicyRevision(t.Context(), f.project.OrganizationID, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.applied && after.Policy.ID != selected.ID || !test.applied && !reflect.DeepEqual(after, prior) {
				t.Fatalf("adoption=%+v, want applied=%t", after, test.applied)
			}
		})
	}
}
