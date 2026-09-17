package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

// Supply authoritative empty field connections for fixtures focused on scheduler
// evidence rather than custom values. Tests for custom fields override these.
func addHydratedProjectFields(data map[string]any, variables map[string]any) {
	for key, id := range variables {
		if strings.HasPrefix(key, "item") {
			data[key] = map[string]any{"id": id, "fieldValues": map[string]any{"nodes": []any{}}}
		}
	}
}

func TestActiveProjectFieldsUseBoundedHydration(t *testing.T) {
	for _, count := range []int{1, 25, 26, 51} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			membership, hydration, boards := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				data := map[string]any{}
				switch {
				case strings.Contains(req.Query, "ProjectFieldItems"):
					membership++
					if strings.Contains(req.Query, "fieldValues(") {
						t.Error("membership query nests custom fields")
					}
					n := 0
					for key, id := range req.Variables {
						if !strings.HasPrefix(key, "id") {
							continue
						}
						n++
						data["issue"+strings.TrimPrefix(key, "id")] = map[string]any{"id": id, "projectItems": map[string]any{"nodes": []any{map[string]any{"id": "P" + id.(string), "project": map[string]string{"id": "PVT_1"}}}}}
					}
					if n > 25 {
						t.Errorf("membership batch %d", n)
					}
				case strings.Contains(req.Query, "ProjectFieldHydration"):
					hydration++
					if len(req.Variables) > 25 {
						t.Errorf("field batch %d", len(req.Variables))
					}
					for key, id := range req.Variables {
						data[key] = map[string]any{"id": id, "statusValue": map[string]string{"name": "In Progress"}, "fieldValues": map[string]any{"nodes": []any{map[string]any{"__typename": "ProjectV2ItemFieldTextValue", "text": "agents", "updatedAt": "2026-09-17T12:00:00Z", "field": map[string]string{"name": "Team"}}}}}
					}
				default:
					boards++
					t.Errorf("unexpected query %s", req.Query)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1"})
			ids := make([]string, count)
			for i := range ids {
				ids[i] = fmt.Sprintf("I%d", i)
			}
			for range 2 {
				if err := c.ensureProjectFieldsCached(t.Context(), ids); err != nil {
					t.Fatal(err)
				}
			}
			if membership != (count+24)/25 || hydration != membership || boards != 0 {
				t.Fatalf("membership=%d hydration=%d boards=%d", membership, hydration, boards)
			}
			for _, id := range ids {
				fields, present, known := c.projectCache.GetProjectFields("PVT_1", id)
				if !present || !known || fields.statusName != "In Progress" || fields.fields["Team"] != "agents" || fields.fields[projectFieldUpdatedAtKeyPrefix+"Team"] != "2026-09-17T12:00:00Z" {
					t.Fatalf("fields for %s: %+v", id, fields)
				}
			}
		})
	}
}

func TestActiveStatesAndRefreshEnumerateBoardOnce(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			boards := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if strings.HasSuffix(r.URL.Path, "/issues/1") {
						fmt.Fprint(w, `{"node_id":"I1","number":1,"title":"Active","state":"open","body":"body","html_url":"https://github.com/owner/repo/issues/1"}`)
					} else {
						fmt.Fprint(w, `[]`)
					}
					return
				}
				var req struct {
					Query     string
					Variables map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				data := map[string]any{}
				switch {
				case strings.Contains(req.Query, "ProjectFieldItems"):
					data["issue0"] = map[string]any{"id": "I1", "number": 1, "repository": map[string]string{"nameWithOwner": "owner/repo"}, "projectItems": map[string]any{"nodes": []any{map[string]any{"id": "P1", "project": map[string]string{"id": "PVT_1"}}}}}
				case strings.Contains(req.Query, "ProjectFieldHydration"):
					addHydratedProjectFields(data, req.Variables)
					data["item0"].(map[string]any)["statusValue"] = map[string]string{"name": "Todo"}
				case strings.Contains(req.Query, "CandidateHydration"):
					addHydratedProjectFields(data, req.Variables)
					data["issue0"] = map[string]any{"id": "I1", "body": "body", "comments": map[string]any{"totalCount": 0}, "blockedBy": map[string]any{"nodes": []any{}}}
				case strings.Contains(req.Query, "ObservedStatusProjectItems"):
					boards++
					if strings.Contains(req.Query, "fieldValues(") {
						t.Error("board contains field connection")
					}
					data["node"] = map[string]any{"items": map[string]any{"totalCount": 1, "nodes": []any{map[string]any{"id": "P1", "statusValue": map[string]string{"name": "Todo"}, "content": map[string]any{"__typename": "Issue", "id": "I1", "number": 1, "title": "Active", "state": "OPEN", "repository": map[string]string{"nameWithOwner": "owner/repo"}}}}}}
				default:
					t.Errorf("unexpected query: %s", req.Query)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			c := newGitHubTestConnector(t, &graphqlTestServer{Server: server}, Config{ProjectSlug: "PVT_1", Repository: "owner/repo", ActiveStates: []string{"Todo"}})
			if warm {
				if err := c.ensureProjectFieldsCached(t.Context(), []string{"I1"}); err != nil {
					t.Fatal(err)
				}
			}
			// Tick reconciles active runs before acquiring the refresh scan slot.
			for range 2 {
				boards = 0
				issues, err := c.FetchIssueStatesByIDs(t.Context(), []string{"I1"})
				if err != nil || len(issues) != 1 {
					t.Fatalf("active states=%v err=%v", issues, err)
				}
				if boards != 0 {
					t.Fatalf("active states enumerated board %d times", boards)
				}
				release, err := c.BeginRefreshScan(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				result := c.FetchRefreshIssues(t.Context(), []string{"Todo"}, nil, connector.IssueFilterHint{})
				release()
				if result.CandidateError != nil || result.StatusError != nil || len(result.Candidates) != 1 {
					t.Fatalf("refresh=%+v", result)
				}
				if boards != 1 {
					t.Fatalf("board enumerations per tick=%d, want 1", boards)
				}
			}
		})
	}
}

func TestDecodeProjectFields(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		valid      bool
	}{
		{"empty connection", `{"id":"P1","fieldValues":{"nodes":[]}}`, true},
		{"missing alias", "", false},
		{"null item", "null", false},
		{"different item", `{"id":"P2","fieldValues":{"nodes":[]}}`, false},
		{"missing connection", `{"id":"P1"}`, false},
		{"malformed item", `{`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := decodeProjectFields(map[string]json.RawMessage{"item0": json.RawMessage(tt.body)}, []string{"I1"}, map[string]string{"I1": "P1"})
			if (err == nil) != tt.valid {
				t.Fatalf("fields=%v error=%v", fields, err)
			}
		})
	}
}
