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
	var output bytes.Buffer
	if err := stagingSmoke(t.Context(), func(string) string { return "" }, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "skipped: DETENT_STAGING_API_KEY is absent") {
		t.Fatal(output.String())
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
	for _, endpoint := range []string{"https://staging.cloud.detent.build/organizations/org_smoke/mcp", "https://staging.cloud.detent.build/mcp"} {
		if actual, err := smokeEndpoint(endpoint); err != nil || actual != endpoint {
			t.Fatal(actual, err)
		}
	}
	for _, endpoint := range []string{"", "https://cloud.detent.build/organizations/org_smoke/mcp", "http://staging.cloud.detent.build/mcp", "https://staging.cloud.detent.build/mcp?token=secret", "https://user@staging.cloud.detent.build/mcp"} {
		if _, err := smokeEndpoint(endpoint); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
}
