package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestDeployReadiness(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, version, commit string
		healthAfter, mcpAfter time.Duration
		wantErr               bool
	}{
		{name: "ready immediately", version: "1.2.3", commit: "expected"},
		{name: "own restart and MCP startup", version: "v1.2.3", commit: "expected", healthAfter: 30 * time.Second, mcpAfter: 34 * time.Second},
		{name: "wrong version", version: "1.2.2", commit: "expected", wantErr: true},
		{name: "wrong commit", version: "1.2.3", commit: "wrong", wantErr: true},
		{name: "Hub stays unavailable", healthAfter: 3 * time.Minute, wantErr: true},
		{name: "MCP stays unavailable", version: "1.2.3", commit: "expected", mcpAfter: 3 * time.Minute, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				mcpCalls := 0
				f := &fakeCloud{}
				handler := mcp.NewHTTPHandler(f, "test", mcp.HTTPConfig{Principal: func(*http.Request) operatortool.Identity {
					return operatortool.Identity{PrincipalID: "operator", OrganizationID: "org", CredentialID: "scoped-key"}
				}})
				client := &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/health" {
						if r.Header.Get("Authorization") != "" {
							t.Fatal("health request exposed smoke credential")
						}
						if time.Since(start) < test.healthAfter {
							w.WriteHeader(http.StatusBadGateway)
							return
						}
						if err := json.NewEncoder(w).Encode(map[string]string{"version": test.version, "commit": test.commit}); err != nil {
							t.Fatal(err)
						}
						return
					}
					mcpCalls++
					if time.Since(start) < test.mcpAfter {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					handler.ServeHTTP(w, r)
				})}}
				transport := &cloudMCP{endpoint: "https://example.test/organizations/org/mcp", token: "test-key", client: client}
				var output bytes.Buffer
				err := waitForDeploy(t.Context(), transport, "v1.2.3", "expected", &output)
				if (err != nil) != test.wantErr {
					t.Fatalf("readiness = %v", err)
				}
				if test.wantErr {
					if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 2*time.Minute || strings.Contains(output.String(), "PASS") {
						t.Fatalf("readiness did not fail at deadline: %v after %s", err, time.Since(start))
					}
					if test.mcpAfter == 0 && mcpCalls != 0 {
						t.Fatal("wrong build reached MCP smoke")
					}
				} else if time.Since(start) != max(test.healthAfter, test.mcpAfter) || len(transport.tools) == 0 || !strings.Contains(output.String(), "PASS") {
					t.Fatal("readiness did not wait for identity and MCP initialization")
				}
			})
		})
	}
}

func TestStagingSmokePolicy(t *testing.T) {
	if err := smokePolicy(operatortool.Registry()); err != nil {
		t.Fatal(err)
	}
	definitions := operatortool.Registry()
	definitions = append(definitions, operatortool.Definition{Name: "new_write"})
	if err := smokePolicy(definitions); err == nil || !strings.Contains(err.Error(), "new_write") {
		t.Fatalf("unclassified write: %v", err)
	}
	for i, definition := range definitions {
		if definition.Name == smokeSkipped[0] {
			definitions[i].Name = "renamed"
			break
		}
	}
	if err := smokePolicy(definitions); err == nil || !strings.Contains(err.Error(), smokeSkipped[0]) {
		t.Fatalf("renamed skip: %v", err)
	}
}

func TestStagingSmokeAbsentSecret(t *testing.T) {
	for _, environment := range []string{"staging", "production"} {
		var output bytes.Buffer
		err := deploySmoke(t.Context(), environment, func(string) string { return "" }, &output)
		if err == nil || !strings.Contains(err.Error(), "DETENT_SMOKE_API_KEY is absent") {
			t.Fatalf("%s absent key = %v", environment, err)
		}
	}
}

type smokeTransport func(*http.Request) (*http.Response, error)

func (f smokeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStagingSmokeRevisionContract(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		revision   any
		failure    string
	}{
		{"string advances", "string", "2", ""},
		{"integer advances", "integer", float64(2), ""},
		{"unchanged", "string", "1", "revision did not advance"},
		{"missing", "string", nil, "revision did not advance"},
		{"decoder failure", "integer", nil, "expected_revision: must be a string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			token := "private-token"
			m := &cloudMCP{token: token, endpoint: "https://example.test/mcp", diagnostic: true, tools: map[string]operatortool.Definition{}}
			m.tools[operatortool.EditItem] = operatortool.Definition{Name: operatortool.EditItem, InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"string"},"identifier":{"type":"string"},"expected_revision":{"type":"` + test.kind + `"},"request_id":{"type":"string"}}}`)}
			m.client = &http.Client{Transport: smokeTransport(func(r *http.Request) (*http.Response, error) {
				var message struct {
					ID     int `json:"id"`
					Params struct {
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
					t.Fatal(err)
				}
				value := message.Params.Arguments["expected_revision"]
				if test.kind == "string" && value != "1" || test.kind == "integer" && value != float64(1) {
					t.Fatalf("wire revision=%#v", value)
				}
				result := map[string]any{"structuredContent": map[string]any{"revision": test.revision}}
				if test.name == "decoder failure" {
					result = map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "expected_revision: must be a string " + token}}}
				}
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: http.Header{}}, nil
			})}
			var output bytes.Buffer
			s := &mcpSmoke{mcp: m, output: &output, project: "prj_smoke", item: "wi_smoke", called: map[string]bool{}, fixtures: map[string]any{}}
			_, _, err := s.mutate(context.Background(), operatortool.EditItem, map[string]any{}, 1)
			if test.failure == "" && err != nil {
				t.Fatal(err)
			}
			if test.failure != "" && (err == nil || !strings.Contains(err.Error(), test.failure)) {
				t.Fatalf("failure=%v", err)
			}
			if strings.Contains(output.String(), token) {
				t.Fatal("secret leaked")
			}
			if test.name == "decoder failure" && (!strings.Contains(err.Error(), `"type":"integer"`) || !strings.Contains(err.Error(), operatortool.EditItem)) {
				t.Fatal("failure omitted tool or schema")
			}
		})
	}
}

func TestStagingSmokeHostedGapDoesNotHideDecoderFailure(t *testing.T) {
	s := &mcpSmoke{mcp: &cloudMCP{tools: map[string]operatortool.Definition{}}, output: io.Discard, called: map[string]bool{}}
	if _, revision, err := s.mutate(t.Context(), operatortool.ArchiveItem, map[string]any{}, 5); err != nil || revision != 5 {
		t.Fatal(revision, err)
	}
	if _, _, err := s.mutate(t.Context(), operatortool.EditItem, map[string]any{}, 5); err == nil || errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStagingSmokeWriteCycle(t *testing.T) {
	operatorReads := []string{operatortool.GetChange, operatortool.GetChangeVersion, operatortool.GetNativeRun}
	fixtureDetails := strings.Fields(`get_conversation get_conversation_attachment list_conversation_messages stream_conversation_events change_viewed_files get_artifact_reference get_attempt_diff github_scope_timings work_attempt_receipt get_runner_capacity get_runner_update get_runner_routing runner_project_diagnostics read_attachment read_attachment_metadata`)
	fixtureReads := strings.Fields(`work_bulk_export list_project_conversations get_conversation get_conversation_attachment list_conversation_messages stream_conversation_events list_changes get_change get_change_version change_viewed_files artifact_references get_artifact_reference work_runs get_attempt_diff get_native_run github_scope_timings work_attempt_receipt get_runner_capacity get_runner_update get_runner_routing runner_project_diagnostics get_project_policy board_session_history get_cutover_receipt get_change_review_policy action_result`)
	for _, test := range []struct {
		name, policyCode, policyMessage               string
		gaps, failure, emptyFixtures, automaticIntake bool
		readFailure                                   string
		unconfirmed                                   bool
		triageFirst                                   bool
		challenge                                     string
		iconFailure                                   string
	}{
		{name: "approved policy"},
		{name: "icon not persisted", iconFailure: "missing", failure: true},
		{name: "icon revision unchanged", iconFailure: "revision", failure: true},
		{name: "icon changes project name", iconFailure: "name", failure: true},
		{name: "prior smoke duplicate", challenge: "valid"},
		{name: "dependency duplicate", challenge: "dependency"},
		{name: "live dependency duplicate", challenge: "live dependency", failure: true},
		{name: "missing duplicate token", challenge: "missing token", failure: true},
		{name: "invalid duplicate number", challenge: "invalid number", failure: true},
		{name: "unrelated duplicate", challenge: "unrelated", failure: true},
		{name: "confirmation refused", challenge: "refused", failure: true},
		{name: "confirmation still challenged", challenge: "repeated", failure: true},
		{name: "reserved Triage first", triageFirst: true},
		{name: "service smoke key", unconfirmed: true, failure: true},
		{name: "automatic native intake", automaticIntake: true},
		{name: "unprovisioned fixtures", emptyFixtures: true},
		{name: "unprovisioned fixtures with real error", emptyFixtures: true, readFailure: operatortool.RunnerFleet, failure: true},
		{name: "available fixture with real error", readFailure: "get_conversation", failure: true},
		{name: "bulk export service failure blocks", readFailure: operatortool.WorkBulkExport, failure: true},
		{name: "queue service failure blocks", readFailure: operatortool.OrderItem, failure: true},
		{name: "session service failure blocks", readFailure: operatortool.BoardSessionHistory, failure: true},
		{name: "cutover service failure blocks", readFailure: "get_cutover_receipt", failure: true},
		{name: "review policy service failure blocks", readFailure: operatortool.GetChangeReviewPolicy, failure: true},
		{name: "fresh project", policyCode: "policy_mismatch", policyMessage: "No approved repository policy"},
		{name: "hosted gaps", gaps: true},
		{name: "different policy refusal", policyCode: "policy_mismatch", policyMessage: "Approved policy has changed", failure: true},
		{name: "wrong refusal code", policyCode: "forbidden", policyMessage: "No approved repository policy", failure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gaps := test.gaps
			revision := int64(0)
			commentRevision := int64(0)
			integrationRevision := int64(1)
			var projectIcon map[string]any
			archived := false
			calls := map[string]int{}
			var filing map[string]any
			m := &cloudMCP{endpoint: "https://example.test/mcp", diagnostic: true, tools: map[string]operatortool.Definition{}}
			for _, definition := range operatortool.Registry() {
				if slices.Contains(smokeWrites, definition.Name) || slices.Contains(fixtureReads, definition.Name) || slices.Contains([]string{"list_projects", "get_project_integration", operatortool.WorkList, operatortool.WorkItem, operatortool.RunnerFleet, operatortool.GetRunnerRouting, operatortool.ReadAttachment, operatortool.ReadAttachmentMetadata}, definition.Name) {
					if definition.Name == operatortool.SetPriority || gaps && definition.Name == operatortool.ArchiveItem {
						continue
					}
					m.tools[definition.Name] = definition
				}
			}
			m.client = &http.Client{Transport: smokeTransport(func(r *http.Request) (*http.Response, error) {
				var message struct {
					ID     int `json:"id"`
					Params struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
					t.Fatal(err)
				}
				name, args := message.Params.Name, message.Params.Arguments
				calls[name]++
				result := map[string]any{}
				isError := false
				switch name {
				case "list_projects":
					states := []map[string]any{{"name": "Backlog", "transitions": []string{"Blocked"}}, {"name": "Blocked", "transitions": []string{"Cancelled"}}, {"name": "Cancelled", "terminal": true}, {"name": "Done", "terminal": true}}
					if test.triageFirst {
						states = append([]map[string]any{{"name": "Triage", "transitions": []string{"Backlog", "Cancelled"}}}, states...)
					}
					result["data"] = map[string]any{"projects": []map[string]any{{"project_id": "prj_smoke", "organization_id": "org_smoke", "profile": "native", "states": states}}}
				case operatortool.WorkBulkExport:
					if args["project_id"] != "prj_smoke" {
						t.Fatalf("wrong export scope: %v", args)
					}
					result = map[string]any{"project_id": "prj_smoke", "items": []any{}, "total": 0, "truncated": false, "freshness": "available"}
				case operatortool.WorkList:
					result["data"] = map[string]any{"items": []map[string]any{{"work_item_id": "wi_dependency"}}}
					if test.challenge == "dependency" || test.challenge == "live dependency" {
						result = map[string]any{}
					}
				case operatortool.FileIssue:
					if args["state"] == "Triage" {
						t.Logf("ordinary file_issue request: %v; valid direct-create transition is Backlog -> Blocked", args)
						isError = true
						result = map[string]any{"code": "invalid_request", "message": "Triage is reserved for linked GitHub issue intake"}
						break
					}
					if args["project_id"] != "prj_smoke" || args["state"] != "Backlog" {
						t.Fatalf("wrong direct-create scope or state: %v", args)
					}
					if test.challenge != "" && calls[name] <= 2 {
						if calls[name] == 1 {
							filing = args
						} else {
							if args["challenge_token"] != "dup_fixture" || !reflect.DeepEqual(args["not_duplicate_of"], []any{float64(77), float64(78)}) {
								t.Fatalf("wrong duplicate confirmation: %v", args)
							}
							delete(args, "challenge_token")
							delete(args, "not_duplicate_of")
							if !reflect.DeepEqual(args, filing) || args["request_id"] == "" {
								t.Fatalf("confirmation changed request identity or content: %v != %v", args, filing)
							}
							if test.challenge == "refused" {
								isError = true
								result = map[string]any{"code": "access_denied", "message": "confirmation denied"}
								break
							}
						}
						if calls[name] == 1 || test.challenge == "repeated" {
							token, title, number := "dup_fixture", "MCP staging smoke edited prior-run", 77
							switch test.challenge {
							case "missing token":
								token = ""
							case "invalid number":
								number = 0
							case "unrelated":
								title = "Customer production incident"
							case "dependency", "live dependency":
								title = "MCP smoke dependency"
							}
							state := "Done"
							if test.challenge == "live dependency" {
								state = "Backlog"
							}
							result = map[string]any{"resource_id": "", "data": map[string]any{"challenge_token": token, "possible_duplicates": []map[string]any{{"number": number, "title": title, "state": state, "work_item_id": "wi_prior_smoke"}, {"number": 78, "title": title, "state": "Done", "work_item_id": "wi_older_smoke"}}}}
							break
						}
					}
					revision = 1
					result = map[string]any{"resource_id": "wi_smoke", "revision": "1"}
					if (test.challenge == "dependency" || test.challenge == "live dependency") && calls[name] == 2 {
						result["resource_id"] = "wi_dependency"
					}
				case operatortool.AddComment:
					commentRevision = 1
					result = map[string]any{"comment_id": "cmt_smoke", "revision": "1"}
				case operatortool.EditComment:
					if args["expected_revision"] != strconv.FormatInt(commentRevision, 10) {
						t.Fatalf("comment revision=%v", args)
					}
					commentRevision++
					result["revision"] = strconv.FormatInt(commentRevision, 10)
				case operatortool.OrderItem, operatortool.SetQueuePriority:
					if name == test.readFailure {
						break
					}
					if gaps {
						isError = true
						result = map[string]any{"code": "invalid_request", "message": "Queue ordering and priority overrides require a GitHub-compatible project; native projects use issue priority"}
						break
					}
					revision++
					result["revision"] = strconv.FormatInt(revision, 10)
				case operatortool.EditItem, operatortool.SetDependency, operatortool.MoveItem, operatortool.ArchiveItem, operatortool.RestoreItem:
					if name == operatortool.MoveItem && calls[name] == 1 && args["target_state"] != "Blocked" {
						t.Fatalf("wrong smoke transition: %v", args)
					}
					if args["expected_revision"] != strconv.FormatInt(revision, 10) {
						t.Fatalf("%s stale/wrong wire revision: %v, current=%d", name, args, revision)
					}
					if name == operatortool.RestoreItem && !archived {
						t.Fatal("restored before archive")
					}
					if name == operatortool.ArchiveItem {
						archived = true
					}
					if name == operatortool.RestoreItem {
						archived = false
					}
					revision++
					result["revision"] = strconv.FormatInt(revision, 10)
					if name == operatortool.EditItem && !test.unconfirmed {
						result["data"] = map[string]any{"issue_contract": map[string]any{"confirmed_sections": map[string]string{
							"acceptance criteria":   "dcd324b553949d8b626cd815ff51c9bc508dec5b322143d400305529e8b812dc",
							"must not break":        "ed59baf6b8c58d36581feaf6a5b503c359b87bb9183b0a3a40d28181e9dc0366",
							"how we know it worked": "ce78953979c32a131e50a2b8987b4679e8276a773814a8f212fd80e454b461bb",
						}}}
					}
				case "get_project_integration":
					result["data"] = map[string]any{"name": "Smoke project", "revision": strconv.FormatInt(integrationRevision, 10), "intake": "manual", "projection": "native", "repository_enabled": false}
					result["data"].(map[string]any)["project_icon"] = projectIcon
					if calls["set_project_icon"] > 0 && test.iconFailure == "name" {
						result["data"].(map[string]any)["name"] = "Changed project"
					}
					if test.automaticIntake {
						result["data"].(map[string]any)["intake"] = "automatic"
					}
				case "set_project_icon":
					if args["organization_id"] != "org_smoke" || args["project_id"] != "prj_smoke" || args["expected_revision"] != strconv.FormatInt(integrationRevision, 10) || !reflect.DeepEqual(args["icon"], map[string]any{"kind": "lucide", "name": "rocket", "color": "blue"}) {
						t.Fatalf("icon arguments: %v", args)
					}
					if test.iconFailure != "missing" {
						projectIcon = args["icon"].(map[string]any)
					}
					if test.iconFailure != "revision" {
						integrationRevision++
					}
					result = map[string]any{"id": "action_integration", "status": "succeeded", "result": `{}`}
				case "rename_project":
					if args["name"] != "Smoke project" || args["project_id"] != "prj_smoke" || args["expected_revision"] != strconv.FormatInt(integrationRevision, 10) {
						t.Fatalf("rename arguments: %v", args)
					}
					integrationRevision++
					result = map[string]any{"id": "action_integration", "status": "succeeded", "result": `{}`}
				case "update_project_integration":
					input := args["input"].(map[string]any)
					var schema smokeSchema
					if err := json.Unmarshal(m.tools[name].InputSchema, &schema); err != nil {
						t.Fatal(err)
					}
					for _, required := range schema.Properties["input"].Required {
						if _, present := input[required]; !present {
							t.Fatalf("integration update omits required input %s", required)
						}
					}
					var intake any = "manual"
					if test.automaticIntake {
						intake = nil
					}
					if input["expected_revision"] != strconv.FormatInt(integrationRevision, 10) || input["intake"] != intake || input["projection"] != "native" || input["repository_enabled"] != false {
						t.Fatalf("changed integration: %v", input)
					}
					integrationRevision++
					result = map[string]any{"id": "action_integration", "status": "succeeded", "result": `{}`}
				case operatortool.ActionResult:
					if args["action_id"] != "action_integration" {
						t.Fatalf("wrong action selector: %v", args)
					}
					result = map[string]any{"id": "action_integration", "status": "succeeded", "result": `{}`}
				case operatortool.BoardSessionHistory:
					isError = true
					result = map[string]any{"code": "not_found", "message": "No recorded session activity exists for this work item and attempt"}
				case "get_cutover_receipt":
					isError = true
					result = map[string]any{"code": "not_found", "message": "No cutover receipt exists for this project"}
				case operatortool.GetChangeReviewPolicy:
					isError = true
					result = map[string]any{"code": "not_found", "message": "No approved Change review policy exists for this project"}
				case operatortool.RunnerFleet:
					result["runners"] = []map[string]any{{"id": "runner_fixture"}, {"id": "runner_other"}}
				case operatortool.RunnerProjectDiagnostics:
					if args["runner_id"] == nil {
						isError = true
						result = map[string]any{"code": "invalid_request", "message": "Multiple runners report this project; select runner_id"}
					} else if args["runner_id"] != "runner_attempt" {
						t.Fatalf("wrong diagnostic runner selector: %v", args)
					}
				case operatortool.GetRunnerRouting:
					if args["runner_id"] != "runner_fixture" {
						t.Fatalf("undiscovered runner fixture: %v", args)
					}
					result["routing"] = map[string]any{}
				case operatortool.WorkItem:
					if args["reference"] != "wi_dependency" {
						t.Fatalf("read transient item: %v", args)
					}
					result["data"] = map[string]any{"revision": strconv.FormatInt(revision, 10), "body": "[fixture](/organizations/org_smoke/api/v2/projects/prj_smoke/attachments/att_" + strings.Repeat("a", 32) + ")"}
				case "list_project_conversations":
					if args["query"] != "MCP smoke conversation" || args["limit"] != float64(1) {
						t.Fatalf("unscoped conversation discovery: %v", args)
					}
					result["conversations"] = []map[string]any{{"id": "conv_fixture"}}
				case "get_conversation", "list_conversation_messages", "stream_conversation_events", "get_conversation_attachment":
					if args["conversation_id"] != "conv_fixture" || name == "get_conversation_attachment" && args["attachment_id"] != "att_conversation" {
						t.Fatalf("wrong conversation selector: %v", args)
					}
					result["messages"] = []map[string]any{{"attachments": []map[string]any{{"id": "att_conversation"}}}}
				case operatortool.ReadAttachment, operatortool.ReadAttachmentMetadata:
					if args["attachment_id"] != "att_"+strings.Repeat("a", 32) {
						t.Fatalf("wrong issue attachment: %v", args)
					}
				case operatortool.ListChanges:
					result["changes"] = []map[string]any{{"change_id": "change_fixture", "current_version_id": "version_fixture"}}
				case operatortool.GetChange, operatortool.GetChangeVersion, operatortool.ChangeViewedFiles:
					if args["work_item_id"] != "wi_dependency" || args["change_id"] != "change_fixture" || name != operatortool.GetChange && args["version_id"] != "version_fixture" {
						t.Fatalf("wrong change selector: %v", args)
					}
				case operatortool.ArtifactReferences:
					result["artifacts"] = []map[string]any{{"artifact_id": "artifact_fixture", "revision": 7}}
				case operatortool.GetArtifactReference:
					if args["artifact_id"] != "artifact_fixture" || args["revision"] != float64(7) || args["work_item_id"] != "wi_dependency" {
						t.Fatalf("wrong artifact revision: %v", args)
					}
				case operatortool.WorkRuns:
					result["data"] = map[string]any{"items": []map[string]any{{"attempt_id": "attempt_fixture", "runner_id": "runner_attempt"}}}
				case operatortool.GetAttemptDiff, operatortool.GetNativeRun:
					if args["attempt_id"] != "attempt_fixture" || args["work_item_id"] != "wi_dependency" {
						t.Fatalf("wrong attempt selector: %v", args)
					}
				case operatortool.GitHubScopeTimings, operatortool.WorkAttemptReceipt:
					if args["native_attempt_id"] != "attempt_fixture" || args["reference"] != "wi_dependency" || name == operatortool.GitHubScopeTimings && args["runner_id"] != "runner_attempt" {
						t.Fatalf("wrong native receipt selector: %v", args)
					}
				case operatortool.GetRunnerCapacity, operatortool.GetRunnerUpdate:
					if args["runner_id"] != "runner_fixture" {
						t.Fatalf("wrong runner selector: %v", args)
					}
				case "get_project_policy":
					isError = test.policyCode != ""
					result = map[string]any{"code": test.policyCode, "message": test.policyMessage}
				default:
					t.Fatalf("unexpected tool %s", name)
				}
				if test.emptyFixtures {
					switch name {
					case operatortool.RunnerFleet, operatortool.WorkItem, "list_project_conversations", operatortool.ListChanges, operatortool.ArtifactReferences, operatortool.WorkRuns:
						result = map[string]any{}
					}
				}
				if name == operatortool.GetChange || name == operatortool.GetChangeVersion || name == operatortool.GetNativeRun {
					isError = true
					result = map[string]any{"message": "operator access is unavailable"}
				}
				if name == test.readFailure {
					isError = true
					result = map[string]any{"message": "read service failed"}
					if name == operatortool.BoardSessionHistory || name == "get_cutover_receipt" || name == operatortool.GetChangeReviewPolicy {
						result["code"] = "not_found"
					}
				}
				envelope := map[string]any{"structuredContent": result, "isError": isError}
				if name == "get_project_policy" && isError {
					content, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					envelope = map[string]any{"content": []map[string]string{{"type": "text", "text": string(content)}}, "isError": true}
				}
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": envelope})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: http.Header{}}, nil
			})}
			var output bytes.Buffer
			s := &mcpSmoke{mcp: m, output: &output, fixtures: map[string]any{}, called: map[string]bool{}, prefix: "test"}
			if err := s.run(t.Context()); test.failure {
				failedTool := "get_project_policy"
				if test.challenge != "" {
					failedTool = operatortool.FileIssue
				}
				if test.readFailure != "" {
					failedTool = test.readFailure
				}
				if test.unconfirmed {
					failedTool = operatortool.EditItem
				}
				if test.iconFailure != "" {
					failedTool = "set_project_icon"
				}
				if err == nil || !strings.Contains(err.Error(), failedTool) {
					t.Fatalf("unexpected smoke verdict: %v", err)
				}
				if test.challenge != "" {
					wantCalls := 1
					if test.challenge == "refused" || test.challenge == "repeated" {
						wantCalls = 2
					}
					if calls[operatortool.FileIssue] != wantCalls || calls[operatortool.EditItem] != 0 {
						t.Fatalf("unsafe duplicate continuation: %v", calls)
					}
					return
				}
			} else if err != nil {
				t.Fatal(err, output.String())
			}
			wantIntegrationRevision := int64(4)
			if test.iconFailure == "revision" {
				wantIntegrationRevision = 3
			}
			if commentRevision != 2 || integrationRevision != wantIntegrationRevision || calls["set_project_icon"] != 1 || calls[operatortool.SetDependency] != 2 || calls[operatortool.WorkItem] != 1 || !gaps && (!archived || calls[operatortool.RestoreItem] != 1 || calls[operatortool.ArchiveItem] != 2) {
				t.Fatalf("incomplete cycle: calls=%v comment=%d integration=%d archived=%t", calls, commentRevision, integrationRevision, archived)
			}
			if gaps && !strings.Contains(output.String(), "expected hosted gap (#615)") {
				t.Fatal(output.String())
			}
			if gaps && (calls[operatortool.OrderItem] != 1 || calls[operatortool.SetQueuePriority] != 1 || !strings.Contains(output.String(), "order_item expected refusal: invalid_request") || !strings.Contains(output.String(), "set_queue_priority expected refusal: invalid_request")) {
				t.Fatalf("native queue refusals did not exercise both tools: %v\n%s", calls, output.String())
			}
			for _, name := range append(fixtureReads, operatortool.ReadAttachment, operatortool.ReadAttachmentMetadata) {
				if slices.Contains(operatorReads, name) || test.emptyFixtures && slices.Contains(fixtureDetails, name) || test.readFailure == "get_conversation" && name == "get_conversation_attachment" {
					if calls[name] != 0 || !strings.Contains(output.String(), name+" skipped: ") {
						t.Fatalf("read %s lacks explicit skip: calls=%d\n%s", name, calls[name], output.String())
					}
				} else if calls[name] != 1 {
					t.Fatalf("fixture read %s called %d times", name, calls[name])
				}
			}
		})
	}
}

func TestStagingSmokeEndpoint(t *testing.T) {
	for _, tt := range []struct {
		environment, endpoint string
		ok                    bool
	}{
		{"staging", "https://staging.cloud.detent.build/organizations/org_smoke/mcp", true},
		{"staging", "https://staging.cloud.detent.build/mcp", true},
		{"production", "https://cloud.detent.build/organizations/org_smoke/mcp", true},
		{"staging", "", false},
		{"staging", "https://cloud.detent.build/organizations/org_smoke/mcp", false},
		{"production", "https://staging.cloud.detent.build/organizations/org_smoke/mcp", false},
		{"staging", "http://staging.cloud.detent.build/mcp", false},
		{"staging", "https://staging.cloud.detent.build/mcp?token=secret", false},
		{"staging", "https://user@staging.cloud.detent.build/mcp", false},
		{"preview", "https://cloud.detent.build/organizations/org_smoke/mcp", false},
	} {
		actual, err := smokeEndpoint(tt.endpoint, tt.environment)
		if tt.ok && (err != nil || actual != tt.endpoint) || !tt.ok && err == nil {
			t.Fatalf("%s %q = %q, %v", tt.environment, tt.endpoint, actual, err)
		}
	}
}

func TestSmokeVerdictRatchet(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failures []error
		called   map[string]bool
		gapped   []string
		blocking string
	}{
		{"new failure blocks", []error{errors.New("file_issue: MCP server error")}, map[string]bool{"file_issue": true}, nil, "file_issue: MCP server error"},
		{"known gap failing is reported", []error{errors.New("board_session_history: advertised arguments {}; MCP server error")}, map[string]bool{"board_session_history": true}, nil, ""},
		{"fixture read error blocks", []error{errors.New("runner_project_diagnostics: MCP server error")}, map[string]bool{"runner_project_diagnostics": true}, nil, "runner_project_diagnostics: MCP server error"},
		{"provisioned fixture passing succeeds", nil, map[string]bool{"runner_project_diagnostics": true}, nil, ""},
		{"known gap passing blocks", nil, map[string]bool{"get_cutover_receipt": true}, nil, "get_cutover_receipt: known gap #724 now passes"},
		{"missing conversation fixture blocks", []error{errors.New("get_conversation: advertised conversation_id={}; smoke project lacks a conversation_id fixture")}, map[string]bool{}, nil, "get_conversation: advertised"},
		{"hosted gap handled as expected is not a pass", nil, map[string]bool{"order_item": true}, []string{"order_item"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, environment := range []string{"staging", "production"} {
				t.Run(environment, func(t *testing.T) {
					var output bytes.Buffer
					err := smokeVerdict(&output, tt.failures, tt.called, tt.gapped, map[string]string{"board_session_history": "#724", "get_cutover_receipt": "#724", "order_item": "#724"})
					if tt.blocking == "" && err != nil {
						t.Fatalf("verdict = %v, want nil", err)
					}
					if tt.blocking != "" && (err == nil || !strings.Contains(err.Error(), tt.blocking)) {
						t.Fatalf("verdict = %v, want %q", err, tt.blocking)
					}
				})
			}
		})
	}
}

func TestSmokeRemembersDependencyFixturesOnlyFromDependencyReads(t *testing.T) {
	for _, tt := range []struct {
		name   string
		tool   string
		stored bool
	}{
		{"board read of another item", "board_state", false},
		{"dependency change list", operatortool.ListChanges, true},
		{"dependency runs", operatortool.WorkRuns, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &mcpSmoke{fixtures: map[string]any{}}
			s.remember(tt.tool, map[string]any{"items": []any{map[string]any{"change_id": "change_other", "attempt_id": "attempt_other", "runner_id": "runner_other"}}})
			if _, found := s.fixtures["change_id"]; found != tt.stored {
				t.Fatalf("change_id stored = %v, want %v", found, tt.stored)
			}
			if _, found := s.fixtures["native_attempt_id"]; found != tt.stored {
				t.Fatalf("native_attempt_id stored = %v, want %v", found, tt.stored)
			}
			if s.fixtures["runner_id"] != "runner_other" {
				t.Fatalf("runner_id = %v, want unscoped fixture kept", s.fixtures["runner_id"])
			}
		})
	}
}
