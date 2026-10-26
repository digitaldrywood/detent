package hubserver

import (
	"database/sql"
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

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
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
		humanApproval                                                                         bool
		fleet                                                                                 bool
		autoInitial                                                                           bool
		bind, adopt, nativeRevision                                                           bool
		history                                                                               int
		historyLegacy                                                                         string
		landing                                                                               string
		unbound, unmerged, otherBranch, lateLanding                                           bool
		largeHistory                                                                          bool
		changedSource, concurrentReports                                                      bool
	}{
		{name: "native landing applies instructions without reachability report", initial: true, canonical: true, changedInputs: true, landing: "native", active: true, status: http.StatusNoContent, applied: true},
		{name: "bound merged PR applies instructions without reachability report", initial: true, canonical: true, changedInputs: true, landing: "pr", active: true, status: http.StatusNoContent, applied: true},
		{name: "native landing recorded after default branch apply gains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "native", lateLanding: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "merged PR recorded after default branch apply gains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "pr", lateLanding: true, active: true, status: http.StatusNoContent, applied: true},
		{name: "native landing recorded after unlanded observation stays pending", initial: true, canonical: true, changedInputs: true, landing: "native", lateLanding: true, status: http.StatusNoContent},
		{name: "merged PR recorded after unlanded observation stays pending", initial: true, canonical: true, changedInputs: true, landing: "pr", lateLanding: true, status: http.StatusNoContent},
		{name: "native default branch landing retains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "native", status: http.StatusNoContent, applied: true},
		{name: "merged PR default branch landing retains provenance", initial: true, canonical: true, changedInputs: true, reachable: true, landing: "pr", status: http.StatusNoContent, applied: true},
		{name: "native landing on another branch stays pending", initial: true, canonical: true, changedInputs: true, landing: "native", otherBranch: true, status: http.StatusNoContent},
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
		{name: "occupied lane stays pending", initial: true, reachable: true, occupied: true, status: http.StatusUnprocessableEntity},
		{name: "different repository pending", initial: true, reachable: true, wrongRepository: true, status: http.StatusNoContent},
		{name: "unenrolled reporter pending", initial: true, reachable: true, legacy: true, status: http.StatusNoContent},
		{name: "missing commit pending", initial: true, reachable: true, missingRevision: true, status: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", test.name)
			clearNativeFixturePolicy(t, f)
			if !test.bind {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository = ? WHERE id = ?", "example-org/example-project", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			runner.enroll(t)
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
				initialApproval, _, err = readProjectPolicy(t.Context(), f.service.database.db, string(f.project.OrganizationID)+"/"+string(f.project.ID))
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.bind {
				f.service.config.ReconcileBackend = &scriptedReconcileBackend{steps: []reconcileStep{{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_example", Owner: "example-org", Name: "example-project", UpdatedAt: time.Now().UTC()}}}}}
				integration, err := readProjectIntegration(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
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
					change := tracker.ChangeRequest{ID: "change_landed", OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: item.WorkItemID, CurrentVersion: "version_landed", Landed: &tracker.ChangeLanding{VersionID: "version_landed", MergeSHA: source.Commit, BaseRef: baseRef, Actor: tracker.Actor{Kind: "runner", PrincipalID: landingActor}}}
					raw, err := json.Marshal(change)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_requests(id, organization_id, project_id, work_item_id, record_json) VALUES(?,?,?,?,?)", change.ID, f.project.OrganizationID, f.project.ID, item.WorkItemID, string(raw)); err != nil {
						t.Fatal(err)
					}
					version := tracker.ChangeVersion{ID: "version_landed", ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{Repository: "https://github.com/example-org/example-project"}}
					raw, err = json.Marshal(version)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_versions(id, change_id, number, record_json) VALUES(?,?,?,?)", version.ID, change.ID, 1, string(raw)); err != nil {
						t.Fatal(err)
					}
				} else {
					repositoryID, _ := seedProjection(t, f.service.database.db)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE repositories SET github_owner = ?, github_name = ? WHERE id = ?", "example-org", "example-project", repositoryID); err != nil {
						t.Fatal(err)
					}
					if !test.unbound {
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id = NULL WHERE repository_id = ?", repositoryID); err != nil {
							t.Fatal(err)
						}
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id = ? WHERE id = ?", repositoryID, f.project.ID); err != nil {
							t.Fatal(err)
						}
					}
					mergedAt := testTimestamp
					if test.unmerged {
						mergedAt = ""
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE pull_requests SET merge_sha = ?, merged_at = ?, merged_by = ?, base_ref = ?, github_state = 'closed', url = ? WHERE repository_id = ?", source.Commit, mergedAt, "landing-owner", baseRef, "https://github.com/example-org/example-project/pull/1", repositoryID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !test.lateLanding {
				seedLanding()
			}
			token := runner.redemption.Credential
			if test.legacy {
				token = f.worker(t, "legacy")
			}
			var pinned tracker.NativeLease
			worker := ""
			if test.active {
				worker = f.worker(t, "active")
				machine := tracker.MachineID("machine_abc")
				if test.fleet {
					active := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					active.enroll(t)
					worker, machine = active.redemption.Credential, active.binding.MachineID
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, active.identityPath()+"/routing", testHubAdminToken, map[string]any{"expected_revision": 1, "display_name": "Active runner", "state": "active", "capacity_limit": 2, "project_ids": []tracker.ProjectID{f.project.ID}, "isolation_tier": isolation.NativeTrusted}), http.StatusOK)
					if test.humanApproval {
						candidate.Requirements.MachineID = string(runner.binding.MachineID)
						candidate = candidate.WithID()
					}
				} else {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": machine, "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
				}
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
						commitTestAnalytics(t, f.service, func(tx *sql.Tx) {
							if _, err := tx.ExecContext(t.Context(), "INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,?,?)", version.ID, change.ID, version.Number, string(raw)); err != nil {
								t.Fatal(err)
							}
							if _, err := tx.ExecContext(t.Context(), "UPDATE change_requests SET record_json=json_set(record_json,'$.current_version_id',?) WHERE id=?", version.ID, change.ID); err != nil {
								t.Fatal(err)
							}
						})
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
				for _, statement := range []string{
					"CREATE TABLE policy_work_counts(kind TEXT NOT NULL)",
					"CREATE TRIGGER count_policy_dirty AFTER INSERT ON native_board_dirty BEGIN INSERT INTO policy_work_counts VALUES('card'); END",
					"CREATE TRIGGER count_policy_states AFTER UPDATE ON workflow_states BEGIN INSERT INTO policy_work_counts VALUES('state'); END",
					"CREATE TRIGGER count_policy_projects AFTER UPDATE ON projects BEGIN INSERT INTO policy_work_counts VALUES('project'); END",
				} {
					if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, report), test.status)
			}
			if test.lateLanding {
				seedLanding()
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if test.landing == "native" {
					err = recordPolicyLandingProvenance(t.Context(), tx, string(f.project.OrganizationID)+"/"+string(f.project.ID), 0, "example-org/example-project", source.Commit, policyLanding{ChangeID: "change_landed"})
				} else {
					var repositoryID int64
					if err := tx.QueryRowContext(t.Context(), "SELECT id FROM repositories WHERE github_owner = 'example-org' AND github_name = 'example-project'").Scan(&repositoryID); err != nil {
						t.Fatal(err)
					}
					err = recordMergedPullRequestPolicyLanding(t.Context(), tx, repositoryID, normalizedPullRequest{State: "closed", URL: "https://github.com/example-org/example-project/pull/1", MergeSHA: source.Commit, MergedAt: time.Now()})
				}
				if err != nil {
					t.Fatal(errors.Join(err, tx.Rollback()))
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
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
					scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
					page, err := f.service.readBoardIssues(t.Context(), scope, url.Values{"include": {"board"}, "limit": {"2000"}})
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
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM policy_work_counts WHERE kind=?", kind).Scan(&count); err != nil {
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
				approval, _, err := readProjectPolicy(t.Context(), f.service.database.db, string(f.project.OrganizationID)+"/"+string(f.project.ID))
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
				approval, _, err := readProjectPolicy(t.Context(), f.service.database.db, string(f.project.OrganizationID)+"/"+string(f.project.ID))
				if err != nil || approval.Policy.ID != original.ID || strings.HasPrefix(approval.ApprovedBy, "runner_") {
					t.Fatalf("runner replaced administrator approval: %+v %v", approval, err)
				}
				fresh := f.create(t, "Second runner uses reported configuration")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidate.ID, MachineID: runner.binding.MachineID, SessionID: "pending", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: original.ID, Policy: candidate}), http.StatusOK)
			}
			if test.forge != "" && !test.applied {
				approval, _, err := readProjectPolicy(t.Context(), f.service.database.db, string(f.project.OrganizationID)+"/"+string(f.project.ID))
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{WorkItemID: fresh.WorkItemID, PolicyID: candidate.ID, MachineID: "machine_second", SessionID: "unapproved", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}), http.StatusOK)
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
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET workflow_markdown=? WHERE id=?", markdown, f.project.ID); err != nil {
		t.Fatal(err)
	}
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
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET workflow_markdown=? WHERE id=?", test.saved, f.project.ID); err != nil {
				t.Fatal(err)
			}
			selected := resolve(test.selected)
			prior, _, err := readProjectPolicy(t.Context(), f.service.database.reader, string(f.project.OrganizationID)+"/"+string(f.project.ID))
			if err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", runner.redemption.Credential, policy.Observation{Descriptor: selected})
			want := http.StatusNoContent
			if test.name == "unsaved guidance" {
				want = http.StatusConflict
			}
			requireNativeStatus(t, response, want)
			after, _, err := readProjectPolicy(t.Context(), f.service.database.reader, string(f.project.OrganizationID)+"/"+string(f.project.ID))
			if err != nil {
				t.Fatal(err)
			}
			if test.applied && after.Policy.ID != selected.ID || !test.applied && !reflect.DeepEqual(after, prior) {
				t.Fatalf("adoption=%+v, want applied=%t", after, test.applied)
			}
		})
	}
}
