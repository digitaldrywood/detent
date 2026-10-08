package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

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
	for _, gaps := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "hosted gaps"}[gaps], func(t *testing.T) {
			revision := int64(0)
			commentRevision := int64(0)
			integrationRevision := int64(1)
			archived := false
			calls := map[string]int{}
			m := &cloudMCP{endpoint: "https://example.test/mcp", diagnostic: true, tools: map[string]operatortool.Definition{}}
			for _, definition := range operatortool.Registry() {
				if slices.Contains(smokeWrites, definition.Name) || slices.Contains([]string{"list_projects", "get_project_integration", operatortool.WorkList, operatortool.WorkItem, operatortool.RunnerFleet, operatortool.GetRunnerRouting}, definition.Name) {
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
					result["data"] = map[string]any{"projects": []map[string]any{{"project_id": "prj_smoke", "organization_id": "org_smoke", "profile": "native", "states": []map[string]any{{"name": "Backlog", "transitions": []string{"Blocked"}}, {"name": "Blocked", "transitions": []string{"Cancelled"}}, {"name": "Cancelled", "terminal": true}}}}}
				case operatortool.WorkList:
					result["data"] = map[string]any{"items": []map[string]any{{"work_item_id": "wi_dependency"}}}
				case operatortool.FileIssue:
					revision = 1
					result = map[string]any{"resource_id": "wi_smoke", "revision": "1"}
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
					if gaps {
						isError = true
						result = map[string]any{"message": "application service is unavailable"}
						break
					}
					revision++
					result["revision"] = strconv.FormatInt(revision, 10)
				case operatortool.EditItem, operatortool.SetDependency, operatortool.MoveItem, operatortool.ArchiveItem, operatortool.RestoreItem:
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
				case "get_project_integration":
					result["data"] = map[string]any{"revision": strconv.FormatInt(integrationRevision, 10), "intake": "manual", "projection": "native", "repository_enabled": false}
				case "update_project_integration":
					input := args["input"].(map[string]any)
					if input["expected_revision"] != strconv.FormatInt(integrationRevision, 10) || input["intake"] != "manual" || input["projection"] != "native" || input["repository_enabled"] != false {
						t.Fatalf("changed integration: %v", input)
					}
					integrationRevision++
					result["revision"] = strconv.FormatInt(integrationRevision, 10)
				case operatortool.RunnerFleet:
					result["runners"] = []map[string]any{{"id": "runner_fixture"}}
				case operatortool.GetRunnerRouting:
					if args["runner_id"] != "runner_fixture" {
						t.Fatalf("undiscovered runner fixture: %v", args)
					}
					result["routing"] = map[string]any{}
				case operatortool.WorkItem:
					result["data"] = map[string]any{"revision": strconv.FormatInt(revision, 10)}
				default:
					t.Fatalf("unexpected tool %s", name)
				}
				envelope := map[string]any{"structuredContent": result, "isError": isError}
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": envelope})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: http.Header{}}, nil
			})}
			var output bytes.Buffer
			s := &mcpSmoke{mcp: m, output: &output, fixtures: map[string]any{}, called: map[string]bool{}, prefix: "test"}
			if err := s.run(t.Context()); err != nil {
				t.Fatal(err, output.String())
			}
			if commentRevision != 2 || integrationRevision != 2 || calls[operatortool.SetDependency] != 2 || calls[operatortool.WorkItem] != 1 || calls[operatortool.GetRunnerRouting] != 1 || !gaps && (!archived || calls[operatortool.RestoreItem] != 1 || calls[operatortool.ArchiveItem] != 2) {
				t.Fatalf("incomplete cycle: calls=%v comment=%d integration=%d archived=%t", calls, commentRevision, integrationRevision, archived)
			}
			if gaps && !strings.Contains(output.String(), "expected hosted gap (#615)") {
				t.Fatal(output.String())
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
		{"known schema gap failing is reported", []error{errors.New("get_change schema: missing fixture")}, map[string]bool{}, nil, ""},
		{"known gap passing blocks", nil, map[string]bool{"get_cutover_receipt": true}, nil, "get_cutover_receipt: known gap #724 now passes"},
		{"known gap not reached is reported", []error{errors.New("get_conversation: advertised conversation_id={}; smoke project lacks a conversation_id fixture")}, map[string]bool{}, nil, ""},
		{"hosted gap handled as expected is not a pass", nil, map[string]bool{"order_item": true}, []string{"order_item"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			err := smokeVerdict(&output, tt.failures, tt.called, tt.gapped, smokeKnownGaps["production"])
			if tt.blocking == "" && err != nil {
				t.Fatalf("verdict = %v, want nil", err)
			}
			if tt.blocking != "" && (err == nil || !strings.Contains(err.Error(), tt.blocking)) {
				t.Fatalf("verdict = %v, want %q", err, tt.blocking)
			}
		})
	}
}
