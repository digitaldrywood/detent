package hubserver

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOperatorWorkListOversizedFirstItem(t *testing.T) {
	for _, test := range []struct {
		field   string
		include []string
		links   int
	}{
		{field: "title"},
		{field: "labels"},
		{field: "issue_contract"},
		{field: "related"},
		{field: "related", links: 300},
		{field: "title", include: []string{"summary"}},
		{field: "labels", include: []string{"summary"}},
		{field: "latest_transition_reason", include: []string{"summary"}},
		{field: "title", include: []string{"work"}},
		{field: "related", include: []string{"work"}},
	} {
		t.Run(fmt.Sprintf("%s/%s/links=%d", test.field, strings.Join(test.include, ","), test.links), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			issues := seedArchiveIssues(t, f.service, scope, 2, "Todo")
			for i, issue := range issues {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override=? WHERE issue_id=(SELECT id FROM issues WHERE native_id=?)", i, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			large := strings.Repeat("<界", operatortool.WorkListPageBytes/4)
			column, value := test.field, large
			switch test.field {
			case "labels":
				column = "labels_json"
				raw, err := json.Marshal([]string{large})
				if err != nil {
					t.Fatal(err)
				}
				value = string(raw)
			case "issue_contract":
				column = "issue_contract_json"
				raw, err := json.Marshal(map[string]string{"human_action": large})
				if err != nil {
					t.Fatal(err)
				}
				value = string(raw)
			case "related":
				column, value = "title", "Example item"
				if test.links > 0 {
					seedArchiveIssues(t, f.service, scope, test.links, "Todo")
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE project_id=? AND number>?", f.project.ID, issues[1].Number); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issue_relationships(first_issue_id,second_issue_id,relationship_type,created_at) SELECT a.id,b.id,'related',? FROM issues a,issues b WHERE a.native_id=? AND b.project_id=a.project_id AND b.number>?`, formatHubTime(f.service.config.now()), issues[0].WorkItemID, issues[1].Number); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET title=? WHERE native_id=?", large, issues[1].WorkItemID); err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO issue_relationships(first_issue_id,second_issue_id,relationship_type,created_at) SELECT a.id,b.id,'related',? FROM issues a,issues b WHERE a.native_id=? AND b.native_id=?`, formatHubTime(f.service.config.now()), issues[0].WorkItemID, issues[1].WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
			case "latest_transition_reason":
				column, value = "title", "Example item"
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := appendNativeHistory(t.Context(), tx, scope, string(issues[0].WorkItemID), "workflow.transitioned", tracker.CollaborationData{ReasonDetail: large}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
				if err := f.service.database.commit(t.Context(), tx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET "+column+"=? WHERE native_id=?", value, issues[0].WorkItemID); err != nil {
				t.Fatal(err)
			}
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			call := hostedContextProtocol(t, f.service, ctx, "http")
			args := map[string]any{"project_id": string(f.project.ID), "limit": 1}
			if len(test.include) > 0 {
				args["include"] = test.include
			}
			for i, want := range issues {
				raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
				var page operatortool.WorkReadResult[tracker.Page[struct {
					Number        int      `json:"number"`
					OmittedFields []string `json:"omitted_fields"`
				}]]
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				if len(raw) > operatortool.MaxResultBytes || len(page.Data.Items) != 1 || page.Data.Items[0].Number != want.Number || i == 0 && !slices.Contains(page.Data.Items[0].OmittedFields, test.field) {
					t.Fatalf("bounded item: bytes=%d page=%+v", len(raw), page.Data)
				}
				var envelope struct {
					Data operatortool.NativeWorkPage `json:"data"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				if envelope.Data.Total != len(issues) {
					t.Fatalf("bounded page total=%d want=%d", envelope.Data.Total, len(issues))
				}
				projections := []any{envelope.Data.Page}
				if work := envelope.Data.Work; work != nil {
					if len(work.Items) == 0 {
						t.Fatal("oversized work summary lost its first item")
					}
					var total int
					for _, lane := range work.Lanes {
						total += lane.Total
					}
					if total != len(issues) {
						t.Fatalf("bounded work total=%d want=%d", total, len(issues))
					}
					projections = append(projections, tracker.Page[operatortool.NativeItem]{Items: work.Items})
				}
				for _, projection := range projections {
					bounded, err := json.Marshal(projection)
					if err != nil || len(bounded) > operatortool.WorkListPageBytes {
						t.Fatalf("projection exceeds budget: bytes=%d error=%v", len(bounded), err)
					}
				}
				if (page.Data.NextCursor == "") != (i == len(issues)-1) {
					t.Fatalf("page %d cursor=%q", i, page.Data.NextCursor)
				}
				args["cursor"] = page.Data.NextCursor
			}
		})
	}
}

func TestOperatorWorkListSummary(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	short := tracker.ProjectID("p")
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO projects
(id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled)
SELECT ?, organization_id, 'Example', profile, states_json, require_dependencies, created_at, github_repository_enabled FROM projects WHERE id=?`, short, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO workflow_states
(project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT ?, source_name, detent_state, terminal, dispatchable, created_at, updated_at FROM workflow_states WHERE project_id=?`, short, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO token_grants (token_id, organization_id, project_id) VALUES (?, ?, ?)", defaultHubOperatorID, f.project.OrganizationID, short); err != nil {
		t.Fatal(err)
	}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	reasons := map[tracker.ProjectID]map[int]string{}
	for _, project := range []tracker.ProjectID{short, f.project.ID} {
		reasons[project] = map[int]string{}
		scope := nativeScope{organization: f.project.OrganizationID, project: project}
		issues := seedArchiveIssues(t, f.service, scope, 40, "Todo")
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i, issue := range issues {
			if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET title=?, issue_type='bug', last_activity_at=? WHERE native_id=?", fmt.Sprintf("Fix %d", i), formatHubTime(now), issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			for _, reason := range []string{"old", "ready"} {
				if i == 1 {
					continue
				}
				if i == 0 {
					reason = ""
				}
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Todo", Reason: "operator", ReasonDetail: reason}, now); err != nil {
					t.Fatal(err)
				}
				reasons[project][issue.Number] = reason
				if reason == "" {
					reasons[project][issue.Number] = "operator"
				}
			}
		}
		if err := f.service.database.commit(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
	}
	wantKeys := []string{"identifier", "number", "state", "priority", "title", "type", "labels", "last_activity_at", "revision", "latest_transition_reason"}
	slices.Sort(wantKeys)
	for _, test := range []struct {
		name      string
		project   tracker.ProjectID
		transport string
		compact   bool
		maxBytes  int
	}{
		{"short identifiers HTTP", short, "http", true, 8 << 10},
		{"short identifiers stdio", short, "stdio", true, 8 << 10},
		{"Cloud identifiers", f.project.ID, "http", true, 12 << 10},
		{"default unchanged", short, "http", false, operatortool.MaxResultBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			call := hostedContextProtocol(t, f.service, ctx, test.transport)
			args := map[string]any{"project_id": string(test.project), "limit": 40}
			if test.compact {
				args["include"] = []string{"summary"}
			}
			raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
			var result operatortool.WorkReadResult[tracker.Page[map[string]json.RawMessage]]
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Data.Items) != 40 || result.Data.NextCursor != "" || len(raw) >= test.maxBytes {
				t.Fatalf("items=%d cursor=%q bytes=%d limit=%d", len(result.Data.Items), result.Data.NextCursor, len(raw), test.maxBytes)
			}
			for _, item := range result.Data.Items {
				if !test.compact {
					if item["actor"] == nil || item["issue_contract"] == nil || item["blocked_reason"] == nil {
						t.Fatal("default lost its existing fields")
					}
					continue
				}
				keys := make([]string, 0, len(item))
				for key := range item {
					keys = append(keys, key)
				}
				slices.Sort(keys)
				var number int
				var reason string
				if err := json.Unmarshal(item["number"], &number); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(item["latest_transition_reason"], &reason); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(keys, wantKeys) || reason != reasons[test.project][number] || !strings.HasPrefix(string(item["identifier"]), `"`+string(test.project)+"#") {
					t.Fatalf("invalid summary: %s", item)
				}
			}
			t.Logf("40-item %s response: %d bytes", test.name, len(raw))
		})
	}
	call := hostedContextProtocol(t, f.service, ctx, "http")
	args := map[string]any{"project_id": string(short), "limit": 1}
	var first operatortool.WorkReadResult[operatortool.NativeWorkPage]
	if err := json.Unmarshal(hostedContextData(t, call("tools/call", operatortool.WorkList, args), false), &first); err != nil {
		t.Fatal(err)
	}
	args["include"], args["cursor"] = []string{"summary"}, first.Data.NextCursor
	var next operatortool.WorkReadResult[operatortool.NativeSummaryPage]
	if err := json.Unmarshal(hostedContextData(t, call("tools/call", operatortool.WorkList, args), false), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Data.Items) != 1 || next.Data.Items[0].Number == first.Data.Items[0].Number || next.Data.NextCursor == "" {
		t.Fatal("summary continuation lost the default cursor")
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: short}
	for _, id := range []tracker.NativeWorkItemID{first.Data.Items[0].WorkItemID, tracker.NativeWorkItemID("")} {
		if id == "" {
			if err := tx.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE project_id=? AND number=?", short, next.Data.Items[0].Number).Scan(&id); err != nil {
				t.Fatal(err)
			}
		}
		if err := appendNativeHistory(t.Context(), tx, scope, string(id), "workflow.transitioned", tracker.CollaborationData{ReasonDetail: strings.Repeat("<", 8000)}, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	args["limit"] = 40
	delete(args, "cursor")
	seen := map[int]bool{}
	pages := 0
	for {
		raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
		var page operatortool.WorkReadResult[operatortool.NativeSummaryPage]
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if len(raw) > operatortool.WorkListPageBytes || len(page.Data.Items) == 0 || pages == 0 && len(page.Data.Items) != 1 {
			t.Fatalf("byte-budget page: %d bytes, %d items", len(raw), len(page.Data.Items))
		}
		for _, item := range page.Data.Items {
			if seen[item.Number] {
				t.Fatalf("repeated item %d", item.Number)
			}
			seen[item.Number] = true
		}
		pages++
		if page.Data.NextCursor == "" {
			break
		}
		args["cursor"] = page.Data.NextCursor
	}
	if len(seen) != 40 || pages < 2 {
		t.Fatalf("items=%d pages=%d", len(seen), pages)
	}
}
