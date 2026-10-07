package hubserver

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	}{
		{name: "external authored definition stays pending against legacy approval", initial: true, canonical: true, fromLegacy: true, local: true, active: true, status: http.StatusNoContent},
		{name: "first release carries existing default branch approval", initial: true, canonical: true, fromLegacy: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "nil configuration projection carries only authored policy", initial: true, canonical: true, active: true, nilConfiguration: true, forge: "gate", status: http.StatusNoContent, applied: true},
		{name: "nil configuration invented digest is refused", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "digest", status: http.StatusUnprocessableEntity},
		{name: "canonical version carries feature approval", initial: true, canonical: true, status: http.StatusNoContent, applied: true},
		{name: "canonical version preserves active lease", initial: true, canonical: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "canonical version without source stays pending", initial: true, canonical: true, local: true, status: http.StatusNoContent},
		{name: "canonical version wrong repository stays pending", initial: true, canonical: true, wrongRepository: true, status: http.StatusNoContent},
		{name: "copied provenance cannot change gate", initial: true, canonical: true, local: true, active: true, forge: "gate", status: http.StatusNoContent},
		{name: "copied provenance cannot change selector", initial: true, canonical: true, local: true, active: true, forge: "selector", status: http.StatusNoContent},
		{name: "copied provenance cannot change behavior", initial: true, canonical: true, local: true, active: true, forge: "behavior", status: http.StatusNoContent},
		{name: "copied provenance cannot change local grant", initial: true, canonical: true, local: true, active: true, forge: "grant", status: http.StatusUnprocessableEntity},
		{name: "nil configuration cannot change gate", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "gate", status: http.StatusNoContent},
		{name: "nil configuration cannot change selector", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "selector", status: http.StatusNoContent},
		{name: "nil configuration cannot change profile", initial: true, canonical: true, local: true, active: true, nilConfiguration: true, forge: "profile", status: http.StatusNoContent},
		{name: "canonical version with changed inputs pending", initial: true, canonical: true, changedInputs: true, status: http.StatusNoContent},
		{name: "default branch initial definition", reachable: true, status: http.StatusNoContent, applied: true},
		{name: "default branch revision", initial: true, reachable: true, status: http.StatusNoContent, applied: true},
		{name: "previously applied definition records each apply", initial: true, reachable: true, replay: true, status: http.StatusNoContent, applied: true},
		{name: "feature branch pending", initial: true, status: http.StatusNoContent},
		{name: "uncommitted definition pending", initial: true, local: true, reachable: true, status: http.StatusNoContent},
		{name: "occupied lane rejected", initial: true, reachable: true, occupied: true, status: http.StatusUnprocessableEntity},
		{name: "different repository pending", initial: true, reachable: true, wrongRepository: true, status: http.StatusNoContent},
		{name: "unenrolled reporter pending", initial: true, reachable: true, legacy: true, status: http.StatusNoContent},
		{name: "missing commit pending", initial: true, reachable: true, missingRevision: true, status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", test.name)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = ? WHERE id = ?", "acme/orders", f.project.ID); err != nil {
				t.Fatal(err)
			}
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			var canonicalPolicy policy.Descriptor
			original := hubTestPolicy()
			original.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: strings.Repeat("a", 40), States: nativeFixtureStates()}
			if test.canonical {
				workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte("Run the work.\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/orders\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Done: [Todo]\n"), HasConfig: true, ConfigPath: "detent.yaml"})
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
			original = original.WithID()
			historyCount := 0
			if test.initial {
				approveHubTestPolicy(t, f.service, f.base+"/policy", original)
				historyCount++
			}
			item := f.create(t, "Preserve occupied Todo and revision")
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
			candidate = candidate.WithID()
			source := &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("b", 40), DefaultBranch: "develop", DefaultBranchHead: strings.Repeat("c", 40), DefaultBranchReachable: test.reachable}
			if test.local {
				source = nil
			}
			if test.wrongRepository {
				source.Repository = "acme/other"
			}
			token := runner.redemption.Credential
			if test.legacy {
				token = f.worker(t, "legacy")
			}
			var pinned tracker.NativeLease
			worker := ""
			if test.active {
				worker = f.worker(t, "active")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "machine_abc", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: item.WorkItemID, PolicyID: original.ID, MachineID: "machine_abc", SessionID: "session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &pinned)
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &item)
			}
			report := policy.Observation{Descriptor: candidate, Source: source}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), test.status)
			if test.forge != "" && !test.applied {
				approval, err := readProjectPolicy(t.Context(), f.service.database.db, string(f.project.OrganizationID)+"/"+string(f.project.ID))
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
				if setup.Policy == nil || setup.Policy.Policy.ID != candidate.ID || len(setup.ObservedPolicies) != 0 {
					t.Fatalf("default-branch apply left pending approval: %+v", setup)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
			} else if len(setup.ObservedPolicies) != 1 || setup.Policy == nil || setup.Policy.Policy.ID != original.ID || !reflect.DeepEqual(setup.ObservedPolicies[0].Source, source) {
				t.Fatalf("unapplied revision changed approval or lost report: %+v", setup)
			}
			if test.applied && test.canonical && !test.fromLegacy {
				previousSource := *source
				previousSource.Commit = original.Workflow.Revision
				previousReport := policy.Observation{Descriptor: original, Source: &previousSource}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, previousReport), http.StatusNoContent)
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &setup)
				if setup.Policy == nil || setup.Policy.Policy.ID != candidate.ID || len(setup.ObservedPolicies) != 0 {
					t.Fatalf("older runner reverted approval or requested reapproval: %+v", setup)
				}
			}
			if test.active {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusOK)
				policyID, err := f.service.database.leasePolicyID(t.Context(), pinned.ID)
				if err != nil || policyID != original.ID {
					t.Fatalf("lease pin changed: %s, %v", policyID, err)
				}
			}
			if test.replay {
				previousSource := *source
				previousSource.Commit = original.Workflow.Revision
				previousReport := policy.Observation{Descriptor: original, Source: &previousSource}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, previousReport), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), http.StatusNoContent)
				historyCount += 2
			}
			var approval policy.Approval
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/policy", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &approval)
			if len(approval.History) != historyCount {
				t.Fatalf("history count = %d, want %d", len(approval.History), historyCount)
			}
			if test.applied {
				entry := approval.History[0]
				previous := ""
				if test.initial {
					previous = original.SourceDigest
				}
				if entry.Repository != "acme/orders" || entry.Commit != candidate.Workflow.Revision || entry.PreviousDefinitionDigest != previous || entry.DefinitionDigest != candidate.SourceDigest || entry.RunnerID != runner.binding.RunnerID || entry.AppliedBy != runner.binding.RunnerID || entry.AppliedAt == "" {
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
			project, err := readNativeProject(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			wantStates := nativeFixtureStates()
			if test.initial {
				wantStates = original.Workflow.States
			}
			if test.applied {
				wantStates = candidate.Workflow.States
			}
			wantStates = nativeTriageStates(wantStates)
			if err != nil || !reflect.DeepEqual(project.States, wantStates) {
				t.Fatalf("workflow states = %+v, %v; want %+v", project.States, err, wantStates)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var retained = item
			decodeHubResponse(t, response, &retained)
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "machine_second", "hostname": "second", "capacity": 1, "version": "test"}), http.StatusOK)
				fresh := f.create(t, "Claim retained legacy approval after upgrade")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidate.ID, MachineID: "machine_second", SessionID: "unapproved", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusConflict)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: original.ID, MachineID: "machine_second", SessionID: "second", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusOK)
				current, err := native.ProjectPolicy(t.Context())
				if err != nil || current.Policy.ID != original.ID || current.ApprovedBy != approval.ApprovedBy || current.ApprovedAt != approval.ApprovedAt || !reflect.DeepEqual(current.History, approval.History) {
					t.Fatalf("legacy load rewrote approval or history: %+v %v", current, err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/policy", testHubAdminToken, map[string]string{"expected_policy_id": original.ID}), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(pinned.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: pinned.FencingToken, TTLSeconds: 90}), http.StatusConflict)
			}
		})
	}
}
