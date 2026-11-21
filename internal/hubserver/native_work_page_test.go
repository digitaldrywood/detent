package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeWorkListLegacyTerminalHistory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "example-library")
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authentication=%d %v", status, err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: credential.ID, ProjectID: other.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	closed := map[tracker.NativeWorkItemID]time.Time{}
	items := map[tracker.ProjectID][]tracker.NativeIssue{}
	var projectSources []*neutralimporttest.Fixture
	for projectIndex, fixture := range []nativeFixture{f, other} {
		source := coreWorkItemNeutralFixture(t, fixture.project.OrganizationID, fixture.project.ID, false)
		source.Records["native_comments"] = nil
		versions := source.Records["collaboration_versions"][:0]
		for _, row := range source.Records["collaboration_versions"] {
			if strings.HasPrefix(string(row.Values["record_id"].Data), "wi_core_import") {
				versions = append(versions, row)
			}
		}
		source.Records["collaboration_versions"] = versions
		source.Add("workflow_states", map[string]any{"id": 2, "project_id": fixture.project.ID, "detent_state": "Done", "terminal": 1})
		for index, suffix := range []string{"", "_second"} {
			item := tracker.NativeWorkItemID("wi_core_import" + suffix)
			at := now.Add(-time.Duration(index+1) * time.Hour)
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: fixture.project.OrganizationID, ProjectID: fixture.project.ID, WorkItemID: item, Number: 1 + index*2, Revision: 2, Profile: "native"}, Title: "Updated title", Body: coreImportBody(), State: "Done", Terminal: true, ClosedAt: &at, Labels: []string{"needs-review"}, Assignees: []string{"example-assignee"}, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: parseTestTime(t, "2026-10-01T00:00:00Z"), UpdatedAt: at, LastActivityAt: at}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			keys, values, err := neutralimport.VersionObject(raw)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = neutralimport.VersionObjectBytes(keys, values)
			if err != nil {
				t.Fatal(err)
			}
			source.Set("collaboration_versions", map[string]string{"record_id": string(item), "revision": "2"}, map[string]any{"encoding": "snapshot", "record_json": string(raw)})
			data, _ := json.Marshal(tracker.CollaborationData{Revision: 2, FromState: "Todo", ToState: "Done"})
			actor, _ := json.Marshal(issue.Actor)
			source.Set("collaboration_events", map[string]string{"work_item_id": string(item), "type": "issue.edited"}, map[string]any{"type": "workflow.transitioned", "data_json": string(data), "recorded_at": hubapi.FormatHubTime(at)})
			sequence := 2
			if index == 0 {
				for eventIndex := range 400 {
					sequence++
					data, _ := json.Marshal(tracker.CollaborationData{Revision: 2, FromState: "Done", ToState: "Done"})
					source.Add("collaboration_events", map[string]any{"id": fmt.Sprintf("event_same_%d", eventIndex), "organization_id": fixture.project.OrganizationID, "project_id": fixture.project.ID, "work_item_id": item, "sequence": sequence, "type": "workflow.transitioned", "schema_version": 1, "actor_json": string(actor), "data_json": string(data), "event_revision": 2, "recorded_at": hubapi.FormatHubTime(now)})
				}
			}
			source.Set("issues", map[string]string{"native_id": string(item)}, map[string]any{"event_sequence": sequence, "workflow_state_id": 2, "updated_at": hubapi.FormatHubTime(at), "native_updated_at": hubapi.FormatHubTime(at), "last_activity_at": hubapi.FormatHubTime(now)})
			issue.WorkItemID = tracker.NativeWorkItemID(fmt.Sprintf("wi_closure_%d%s", projectIndex, suffix))
			closed[issue.WorkItemID] = at
			items[fixture.project.ID] = append(items[fixture.project.ID], issue)
		}
		for table, rows := range source.Records {
			for rowIndex, row := range rows {
				if projectIndex > 0 && (table == "issues" || table == "queue_entries") {
					oldID := string(row.Values["id"].Data)
					numeric, err := strconv.Atoi(oldID)
					if err != nil {
						t.Fatal(err)
					}
					cell := row.Values["id"]
					cell.Data = []byte(strconv.Itoa(numeric + 100*projectIndex))
					row.Values["id"] = cell
					row.SourceRowID = int64(numeric + 100*projectIndex)
					for keyIndex, key := range row.Key {
						if key == oldID {
							row.Key[keyIndex] = string(cell.Data)
						}
					}
					if table == "queue_entries" {
						cell := row.Values["issue_id"]
						numeric, err := strconv.Atoi(string(cell.Data))
						if err != nil {
							t.Fatal(err)
						}
						cell.Data = []byte(strconv.Itoa(numeric + 100*projectIndex))
						row.Values["issue_id"] = cell
					}
				}
				for column, cell := range row.Values {
					if cell.Null || cell.Encoding == "binary" {
						continue
					}
					value := strings.ReplaceAll(string(cell.Data), "wi_core_import", fmt.Sprintf("wi_closure_%d", projectIndex))
					value = strings.ReplaceAll(value, "event_", fmt.Sprintf("event_closure_%d_", projectIndex))
					value = strings.ReplaceAll(value, "comment_core_import", fmt.Sprintf("comment_closure_%d", projectIndex))
					cell.Data = []byte(value)
					row.Values[column] = cell
				}
				for keyIndex, key := range row.Key {
					key = strings.ReplaceAll(key, "wi_core_import", fmt.Sprintf("wi_closure_%d", projectIndex))
					key = strings.ReplaceAll(key, "event_", fmt.Sprintf("event_closure_%d_", projectIndex))
					row.Key[keyIndex] = strings.ReplaceAll(key, "comment_core_import", fmt.Sprintf("comment_closure_%d", projectIndex))
				}
				source.Records[table][rowIndex] = row
			}
		}
		projectSources = append(projectSources, source)
	}
	bundle := mergeCoreNeutralProjectSources(t, projectSources...).Write()
	for _, fixture := range []nativeFixture{f, other} {
		input := coreimport.CoreWorkItemImportSource{BundlePath: bundle, OrganizationID: fixture.project.OrganizationID, ProjectID: fixture.project.ID, Objects: fixture.service.runDetailStore()}
		if count, err := coreimport.ImportCoreWorkItems(t.Context(), fixture.service.database.postgres, input); err != nil || count != 2 {
			t.Fatalf("historical closure import count=%d error=%v", count, err)
		}
	}
	for _, stage := range []string{"verified historical import", "without archive storage"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "without archive storage" {
				f.service.config.Content = nil
				if f.service.storageArchiveContent() != nil {
					t.Fatal("fixture unexpectedly has archive storage")
				}
			}
			drainTestNativeBoard(t, f.service)
			for _, fixture := range []nativeFixture{f, other} {
				scope := hubscope.NativeScope{Organization: fixture.project.OrganizationID, Project: fixture.project.ID, Credential: credential}
				selection := viewstore.BoardSelection{ProjectID: fixture.project.ID, Terminal: new(true), ClosedOrder: true, Limit: 10}
				if err := fixture.service.config.TenantRepository.ReadTenant(t.Context(), fixture.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					for itemIndex, item := range items[fixture.project.ID] {
						after, total, transitions := int64(0), 0, 0
						for range 3 {
							events, err := r.Events(t.Context(), fixture.project.ID, item.WorkItemID, after, 200)
							if err != nil {
								return err
							}
							for _, event := range events {
								after, total = event.Sequence, total+1
								if event.Type == "workflow.transitioned" {
									transitions++
									from := "Done"
									if event.Sequence == 2 {
										from = "Todo"
									}
									if event.FromState != from || event.ToState != "Done" {
										t.Fatalf("historical transition facts changed: %+v", event)
									}
								}
							}
							if len(events) < 200 {
								break
							}
						}
						wantEvents, wantTransitions := 2, 1
						if itemIndex == 0 {
							wantEvents, wantTransitions = 402, 401
						}
						if total != wantEvents || transitions != wantTransitions {
							t.Fatalf("historical population events=%d transitions=%d want events=%d transitions=%d", total, transitions, wantEvents, wantTransitions)
						}
					}
					rows, err := r.BoardPage(t.Context(), selection)
					if err != nil {
						return err
					}
					if len(rows) != 2 {
						t.Fatalf("closed population=%+v", rows)
					}
					for _, row := range rows {
						if row.ClosedAt == nil || !row.ClosedAt.Equal(closed[row.WorkItemID]) {
							t.Fatalf("closed population item=%+v want=%v", row, closed[row.WorkItemID])
						}
					}
					counts, err := r.BoardClosedWindows(t.Context(), selection, []time.Time{now.Add(-24 * time.Hour)}, now)
					if err != nil {
						return err
					}
					shipped, cancelled := int64(0), int64(0)
					for _, count := range counts {
						if count.State == "Done" {
							shipped += count.Total
						}
						if count.State == "Cancelled" {
							cancelled += count.Total
						}
					}
					if shipped != 2 || cancelled != 0 {
						t.Fatalf("closed population shipped=%d cancelled=%d", shipped, cancelled)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				board := testBoardWork(readTestWorkBoard(t, t.Context(), fixture.service, scope, savedWorkState{View: "board", CompletedWindow: "24h"}))
				if board == nil || board.Completed != 2 || board.Cancelled != 0 {
					t.Fatalf("board summary=%+v", board)
				}
				ctx := changeOperatorContext(t, f.service, f.token, string(fixture.project.OrganizationID))
				call := hostedContextProtocol(t, f.service, ctx, "http")
				for _, include := range []string{"default", "summary", "work"} {
					t.Run(string(fixture.project.ID)+"/"+include, func(t *testing.T) {
						args := map[string]any{"project_id": string(fixture.project.ID), "limit": 1}
						if include != "default" {
							args["include"] = []string{include}
						}
						for index, want := range items[fixture.project.ID] {
							raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
							var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
							if err := json.Unmarshal(raw, &result); err != nil {
								t.Fatal(err)
							}
							page := result.Data
							if page.Total != -1 || len(page.Items) != 1 || page.Items[0].Identifier != string(fixture.project.ID)+"#"+strconv.Itoa(want.Number) || page.Items[0].State != "Done" {
								t.Fatalf("page %d: total=%d items=%d want number=%d", index, page.Total, len(page.Items), want.Number)
							}
							if include == "work" && (page.Items[0].ClosedAt == nil || !page.Items[0].ClosedAt.Equal(closed[want.WorkItemID])) {
								t.Fatalf("closed at=%v want=%v", page.Items[0].ClosedAt, closed[want.WorkItemID])
							}
							if (page.NextCursor == "") != (index == 1) {
								t.Fatalf("page %d cursor=%q", index, page.NextCursor)
							}
							args["cursor"] = page.NextCursor
						}
					})
				}
				params := url.Values{"sort": {"closed"}, "limit": {"2"}}
				page, err := f.service.readIssues(t.Context(), scope, params)
				if err != nil || len(page.Items) != 2 {
					t.Fatalf("closed page=%+v err=%v", page, err)
				}
				for index, issue := range page.Items {
					want := items[fixture.project.ID][index]
					if issue.WorkItemID != want.WorkItemID || issue.ClosedAt == nil || !issue.ClosedAt.Equal(closed[want.WorkItemID]) {
						t.Fatalf("closed item=%+v want=%s at=%v", issue, want.WorkItemID, closed[want.WorkItemID])
					}
				}
			}
		})
	}
}

func updateNativeWorkPageFixtureIssue(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID, update func(*tracker.NativeIssue)) tracker.NativeIssue {
	t.Helper()
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	row, version, err := f.service.readWorkItemRecord(t.Context(), scope, string(id))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := f.service.hydrateCanonicalWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	project, err := f.service.readProjectRecord(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	update(&issue)
	issue.Revision++
	issue.UpdatedAt = f.service.config.Clock().UTC()
	plan := hubrecords.WorkItemRecordPlan{Current: row, Project: project, Issue: issue, Fields: []string{"title", "labels", "assignees", "priority"}}
	fields, err := f.service.prepareWorkItemFields(t.Context(), scope.Organization, plan)
	if err != nil {
		t.Fatal(err)
	}
	version, err = eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: id, RecordID: string(id), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := hubrecords.WriteWorkItemRecord(t.Context(), w, plan, fields); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return issue
}

func indexNativeWorkPageFixtureIssues(t *testing.T, f nativeFixture, _ []tracker.NativeIssue) {
	t.Helper()
	projectNativeItemPages(t, f.service, f.project.OrganizationID)
}

func TestNativeWorkPageOperationalScope(t *testing.T) {
	t.Parallel()
	fixtureNow := time.Now().UTC().Truncate(time.Microsecond)
	details := &memoryRunDetails{keyReads: make(map[string]int)}
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return fixtureNow }, WebhookMaintenanceInterval: time.Hour, RunDetails: details, Postgres: &postgres.Config{Pool: postgres.PoolConfig{SearchBudget: time.Hour}}})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	history := seedArchiveIssues(t, f.service, scope, 130, "Done")
	todo := seedArchiveIssues(t, f.service, scope, 4, "Todo")
	ip := seedArchiveIssues(t, f.service, scope, 2, "In Progress")
	var activeLease tracker.NativeLease
	for index, issue := range ip {
		worker := f.worker(t, "work-page-worker-"+string(issue.WorkItemID))
		lease := claimNativeAttempt(t, f, worker, "work-page-machine-"+string(issue.WorkItemID), "work-page-session-"+string(issue.WorkItemID), issue.WorkItemID)
		start := nativeStartedEvent(lease)
		start.IdempotencyKey = string(issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
		if index == 0 {
			if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=$3,renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2", scope.Organization, lease.ID, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		} else {
			activeLease = lease
		}
	}
	updateNativeWorkPageFixtureIssue(t, f, todo[0].WorkItemID, func(issue *tracker.NativeIssue) {
		issue.Labels, issue.Assignees, issue.Priority = []string{"selected"}, []string{"operator"}, new(0)
	})
	for _, issue := range []tracker.NativeIssue{history[120], todo[1]} {
		updateNativeWorkPageFixtureIssue(t, f, issue.WorkItemID, func(current *tracker.NativeIssue) {
			current.Title, current.Labels, current.Assignees, current.Priority = "Historical needle "+strconv.Itoa(current.Number), []string{"alternate", "Historical label"}, []string{"reviewer"}, new(1)
		})
	}
	updateNativeWorkPageFixtureIssue(t, f, todo[2].WorkItemID, func(issue *tracker.NativeIssue) { issue.Labels = []string{"human-owned"} })
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: todo[3].WorkItemID, RelatedProjectID: scope.Project, RelatedWorkItemID: todo[2].WorkItemID, Type: "dependency", Provenance: "native"}, "add", f.service.config.Clock())
	}); err != nil {
		t.Fatal(err)
	}
	indexNativeWorkPageFixtureIssues(t, f, append(append(slices.Clone(history), todo...), ip...))
	otherProject := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-project")
	otherProject.create(t, "Historical needle")
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx = f.service.withOperatorCatalog(ctx, credential, string(f.project.OrganizationID))
	transports := map[string]func(string, string, any) hostedContextReply{}
	for _, name := range []string{"stdio", "http"} {
		transports[name] = hostedContextProtocol(t, f.service, ctx, name)
	}
	read := func(query string) tracker.NativeIssuePage {
		drainTestNativeBoard(t, f.service)
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+query, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.NativeIssuePage
		decodeHubResponse(t, response, &page)
		params, err := url.ParseQuery("include=work&limit=100" + query)
		if err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"project_id": string(f.project.ID), "include": []string{"work"}, "limit": 100}
		for _, field := range []string{"state", "label", "assignee"} {
			if values := params[field]; len(values) != 0 {
				args[field+"s"] = values
			}
		}
		for _, field := range []string{"cursor", "archived"} {
			if value := params.Get(field); value != "" {
				args[field] = value
			}
		}
		if value := params.Get("q"); value != "" {
			args["query"] = value
		}
		if values := params["priority"]; len(values) != 0 {
			priorities := []int{}
			for _, value := range values {
				priority, err := strconv.Atoi(value)
				if err != nil {
					t.Fatal(err)
				}
				priorities = append(priorities, priority)
			}
			args["priorities"] = priorities
		}
		for transport, call := range transports {
			raw := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
			var result operatortool.WorkReadResult[operatortool.NativeWorkPage]
			if err := json.Unmarshal(raw, &result); err != nil || result.Data.Total != page.Total || result.Data.Work == nil || !reflect.DeepEqual(result.Data.Work.Lanes, page.Work.Lanes) || len(result.Data.Work.Items) != len(page.Work.Items) || len(result.Data.Items) != len(page.Items) {
				t.Fatalf("%s query=%s result=%s err=%v", transport, query, raw, err)
			}
			for index, issue := range page.Items {
				if result.Data.Items[index].WorkItemID != issue.WorkItemID {
					t.Fatalf("%s dropped filter for %s", transport, query)
				}
			}
			for index, issue := range page.Work.Items {
				if result.Data.Work.Items[index].WorkItemID != issue.WorkItemID {
					t.Fatalf("%s lost operational selection for %s", transport, query)
				}
			}
		}
		return page
	}
	first := read("")
	for _, include := range []string{"", "&include=summary"} {
		ids := make(map[tracker.NativeWorkItemID]bool)
		for _, issue := range first.Items {
			ids[issue.WorkItemID] = true
		}
		cursor := first.NextCursor
		for cursor != "" {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=100&cursor="+url.QueryEscape(cursor)+include, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var continuation tracker.NativeIssuePage
			if err := json.Unmarshal(response.Body.Bytes(), &continuation); err != nil || continuation.Work != nil || len(continuation.Items) == 0 || continuation.Total != -1 {
				t.Fatalf("continuation without work: %s err=%v", response.Body, err)
			}
			for _, issue := range continuation.Items {
				if ids[issue.WorkItemID] {
					t.Fatalf("continuation repeated %s", issue.WorkItemID)
				}
				ids[issue.WorkItemID] = true
			}
			cursor = continuation.NextCursor
		}
		if len(ids) != 136 {
			t.Fatalf("continuation lost inventory: got %d items, want 136", len(ids))
		}
	}
	for transport, call := range transports {
		for _, name := range []string{operatortool.Dashboard, operatortool.BoardState} {
			t.Run(transport+"/"+name, func(t *testing.T) {
				catalog := call("tools/list", "", nil).Result
				if !strings.Contains(string(catalog), `"name":"`+name+`"`) {
					t.Fatalf("native read missing from catalog: %s", catalog)
				}
				for _, test := range []struct {
					state  string
					counts nativeBoardCounts
				}{
					{"", nativeBoardCounts{Running: 1, QueuedInventory: 5, Open: 6, ClosedInventory: 130, Total: 136}},
					{"Todo", nativeBoardCounts{QueuedInventory: 4, Open: 4, Total: 4}},
					{"Done", nativeBoardCounts{ClosedInventory: 130, Total: 130}},
				} {
					if name == operatortool.Dashboard && test.state != "" {
						continue
					}
					args := map[string]any{"project_id": string(f.project.ID), "limit": 100}
					if test.state != "" {
						args["state"] = test.state
					}
					raw := hostedContextData(t, call("tools/call", name, args), false)
					var result nativeBoardResult
					if err := json.Unmarshal(raw, &result); err != nil || result.Counts != test.counts || len(result.Projects) != 1 || result.Projects[0].Project.ID != f.project.ID || result.GeneratedAt.IsZero() || result.Truncated != (test.counts.Total > 100) {
						t.Fatalf("native inventory: %s err=%v", raw, err)
					}
					if !slices.Contains(result.Unavailable, "aggregate_dispatch_readiness") || result.EligibilityTool != operatortool.ExplainItem || strings.Contains(string(raw), `"ready"`) || strings.Contains(string(raw), "foreign-project") {
						t.Fatalf("inventory claimed readiness or leaked scope: %s", raw)
					}
					if test.state != "Done" && len(result.Projects[0].Board.Work.Items) != test.counts.Open {
						t.Fatalf("lost bounded operational items: %s", raw)
					}
				}
				hostedContextData(t, call("tools/call", name, map[string]any{"project_id": "prj_foreign"}), true)
				hostedContextData(t, call("tools/call", name, map[string]any{"project_id": string(f.project.ID), "limit": 201}), true)
				raw := hostedContextData(t, call("tools/call", name, map[string]any{"limit": 1}), false)
				var bounded nativeBoardResult
				if err := json.Unmarshal(raw, &bounded); err != nil || !bounded.Truncated || len(bounded.Projects) != 1 || len(bounded.Projects[0].Board.Items) > 1 || len(bounded.Projects[0].Board.Work.Items) > 1 {
					t.Fatalf("unscoped read exceeded bounds: %s err=%v", raw, err)
				}
			})
		}
	}
	readInventory := func(t *testing.T, query string) (int, int) {
		t.Helper()
		params, err := url.ParseQuery("limit=100" + query)
		if err != nil {
			t.Fatal(err)
		}
		params.Del("cursor")
		seen := map[tracker.NativeWorkItemID]bool{}
		running := 0
		for {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.NativeIssuePage
			decodeHubResponse(t, response, &page)
			statusParams := url.Values{}
			for _, issue := range page.Items {
				if seen[issue.WorkItemID] {
					t.Fatalf("inventory repeated %s", issue.WorkItemID)
				}
				seen[issue.WorkItemID] = true
				statusParams.Add("item", string(issue.WorkItemID))
			}
			if len(page.Items) > 0 {
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/board/status?"+statusParams.Encode(), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var statuses []viewstore.BoardStatusRecord
				decodeHubResponse(t, response, &statuses)
				if len(statuses) != len(page.Items) {
					t.Fatalf("inventory status count=%d want=%d", len(statuses), len(page.Items))
				}
				for _, status := range statuses {
					if status.Execution != nil && status.Execution.Running {
						running++
					}
				}
			}
			if page.NextCursor == "" {
				return len(seen), running
			}
			if page.NextCursor == params.Get("cursor") {
				t.Fatal("inventory cursor did not advance")
			}
			params.Set("cursor", page.NextCursor)
		}
	}
	for _, test := range []struct {
		name, query          string
		total, running, open int
	}{
		{"default", "", 136, 1, 6},
		{"history cursor", "&cursor=" + url.QueryEscape(first.NextCursor), 136, 1, 6},
		{"terminal lane", "&state=Done", 130, 0, 0},
		{"dispatchable lane", "&state=Todo", 4, 0, 4},
		{"IP membership", "&state=In%20Progress", 2, 1, 2},
		{"label", "&label=selected", 1, 0, 1},
		{"assignee", "&assignee=operator", 1, 0, 1},
		{"archived", "&archived=true", 0, 0, 0},
		{"multiple states", "&state=Todo&state=Done", 134, 0, 4},
		{"multiple dimensions", "&state=Todo&state=Done&label=selected&label=alternate&assignee=operator&assignee=reviewer&priority=0&priority=1", 3, 0, 2},
		{"older title", "&q=Historical%20needle", 2, 0, 1},
		{"older label", "&q=historical%20LABEL", 2, 0, 1},
		{"older identifier", "&q=" + url.QueryEscape(f.project.Name+"#"+strconv.Itoa(history[120].Number)), 2, 0, 0},
		{"native identifier", "&q=" + url.QueryEscape(string(f.project.ID)+"#"+strconv.Itoa(history[120].Number)), 2, 0, 0},
		{"absent body search", "&q=Preserved%20body", 0, 0, 0},
		{"AND dimensions", "&state=In%20Progress&label=selected&label=alternate", 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := read(test.query)
			if strings.HasSuffix(test.name, "identifier") && !slices.ContainsFunc(page.Items, func(item tracker.NativeIssue) bool { return item.WorkItemID == history[120].WorkItemID }) {
				t.Fatal("identifier search lost legacy match")
			}
			if page.Total != -1 || page.Work == nil || len(page.Items) > 100 || len(page.Work.Items) != test.open || page.Work.Truncated || page.Work.AsOf.IsZero() {
				t.Fatalf("page lost bounds or open selection: %+v", page.Work)
			}
			laneTotal, laneRunning := 0, 0
			for _, lane := range page.Work.Lanes {
				laneTotal, laneRunning = laneTotal+lane.Total, laneRunning+lane.Running
			}
			if total, running := readInventory(t, test.query); total != test.total || running != test.running || (page.Work.Lanes == nil) != strings.Contains(test.query, "q=") || page.Work.Lanes != nil && (laneTotal != total || laneRunning != running) {
				t.Fatalf("scope total=%d running=%d lanes=%+v, want %d/%d", total, running, page.Work.Lanes, test.total, test.running)
			}
			for _, issue := range append(page.Items, page.Work.Items...) {
				if issue.Body != "" || issue.ProjectID != f.project.ID {
					t.Fatal("compact projection loaded a body or foreign project")
				}
			}
		})
	}
	if first.NextCursor == "" || first.Items[0].WorkItemID != todo[0].WorkItemID || first.Work.Items[0].WorkItemID != ip[1].WorkItemID || !slices.ContainsFunc(first.Work.Items, func(issue tracker.NativeIssue) bool { return issue.WorkItemID == todo[0].WorkItemID }) {
		t.Fatal("default lane page omitted operational work")
	}
	now := time.Now().Truncate(time.Second).Add(500 * time.Millisecond)
	f.service.config.Clock = func() time.Time { return now }
	for _, test := range []struct {
		name             string
		renewed, expires time.Time
		running          int
	}{
		{"mixed timestamp precision", now.Truncate(time.Second), now.Add(time.Hour), 1},
		{"expiry boundary", now.Add(-time.Minute), time.Time{}, 0},
		{"unexpired despite clock skew", now.Add(time.Minute), now.Add(time.Hour), 1},
		{"restore current lease", now.Add(-time.Second), now.Add(time.Hour), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var expiry *time.Time
			if !test.expires.IsZero() {
				expiry = &test.expires
			}
			if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$3),renewed_at=$3,expires_at=coalesce($4::timestamptz,transaction_timestamp()) WHERE organization_id=$1 AND lease_id=$2", scope.Organization, activeLease.ID, test.renewed, expiry); err != nil {
				t.Fatal(err)
			}
			if err := f.service.refreshBoardRecord(t.Context(), scope.Organization, viewstore.BoardCandidate{ProjectID: scope.Project, WorkItemID: ip[1].WorkItemID}, now); err != nil {
				t.Fatal(err)
			}
			read("")
			_, running := readInventory(t, "")
			if running != test.running {
				t.Fatalf("running=%d, want %d", running, test.running)
			}
		})
	}
	more := seedArchiveIssues(t, f.service, scope, 130, "Todo")
	for _, issue := range more {
		updateNativeWorkPageFixtureIssue(t, f, issue.WorkItemID, func(current *tracker.NativeIssue) {
			current.Title, current.Labels, current.Assignees, current.Priority = "Open needle", []string{"open-match"}, []string{"reviewer"}, new(1)
		})
	}
	indexNativeWorkPageFixtureIssues(t, f, more)
	query := "&q=Open%20needle&state=Todo&state=Done&label=open-match&label=selected&assignee=operator&assignee=reviewer&priority=0&priority=1"
	matching := read(query)
	total, _ := readInventory(t, query)
	found := map[tracker.NativeWorkItemID]bool{}
	for {
		if total != 130 || len(matching.Items) > 100 || len(matching.Work.Items) > 100 {
			t.Fatalf("matching scope or bounds changed: %+v", matching.Work)
		}
		for _, issue := range matching.Items {
			if issue.ProjectID != f.project.ID || issue.Title != "Open needle" || issue.Body != "" {
				t.Fatalf("matching query leaked an item: %+v", issue)
			}
			found[issue.WorkItemID] = true
		}
		if matching.NextCursor == "" {
			break
		}
		matching = read(query + "&cursor=" + url.QueryEscape(matching.NextCursor))
	}
	drainTestNativeBoard(t, f.service)
	if len(found) != 130 || !found[more[0].WorkItemID] {
		t.Fatal("older open match was unreachable")
	}
	for _, invalid := range []string{
		"&q=one&q=two", "&include=work", "&state=" + strings.Repeat("x", 4097),
		strings.Repeat("&state=Todo", 33), query + "&q=changed&cursor=" + url.QueryEscape(read(query).NextCursor),
	} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+invalid, f.token, nil), http.StatusUnprocessableEntity)
	}
	changed := strings.Replace(query, "state=Todo", "state=In%20Progress", 1)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&limit=100"+changed+"&cursor="+url.QueryEscape(read(query).NextCursor), f.token, nil), http.StatusUnprocessableEntity)
	page := read("")
	if !page.Work.Truncated || len(page.Work.Items) == 0 || len(page.Work.Items) > 100 || page.Work.Items[0].WorkItemID != ip[1].WorkItemID {
		t.Fatal("large open inventory displaced live work or exceeded the bound")
	}
	for _, items := range [][]tracker.NativeIssue{page.Items, page.Work.Items} {
		raw, err := json.Marshal(operatortool.NativeItemPage(string(f.project.ID), tracker.Page[tracker.NativeIssue]{Items: items}))
		if err != nil || len(raw) > operatortool.WorkListPageBytes {
			t.Fatalf("work projection exceeds the existing page budget: bytes=%d error=%v", len(raw), err)
		}
	}
	details.mu.Lock()
	beforeObjectReads, retainedObjects := details.reads, len(details.data)
	details.mu.Unlock()
	if beforeObjectReads == 0 || retainedObjects == 0 {
		t.Fatal("compact read fixture did not exercise its actual object store")
	}
	scope.Credential = credential
	if _, err := f.service.readBoardRecords(t.Context(), scope, url.Values{"include": {"work"}, "limit": {"100"}}); err != nil {
		t.Fatal(err)
	}
	details.mu.Lock()
	objectReads := details.reads - beforeObjectReads
	details.mu.Unlock()
	if objectReads != 0 {
		t.Fatalf("compact read hydrated immutable objects: %d reads", objectReads)
	}
}

func TestNativeWorkPageCompletedWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.October, 6, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
	descriptor := hubTestPolicy()
	descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(nativeFixtureStates(), tracker.NativeState{Name: "Cancelled", Terminal: true}, tracker.NativeState{Name: "Retired", Terminal: true})}
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor.WithID())
	var issues []tracker.NativeIssue
	var histories [][]tracker.CollaborationEvent
	for index, seed := range []struct {
		state    string
		entries  []time.Time
		from     string
		archived bool
	}{
		{"Done", []time.Time{now.Add(-time.Hour)}, "Todo", false},
		{"Retired", []time.Time{now.Add(-time.Hour)}, "Todo", false},
		{"Done", []time.Time{now.Add(-48 * time.Hour)}, "Todo", false},
		{"Done", []time.Time{now.Add(-48*time.Hour - time.Second)}, "Todo", false},
		{"Cancelled", []time.Time{now.Add(-8 * 24 * time.Hour)}, "Todo", false},
		{"Done", []time.Time{now.Add(-15 * 24 * time.Hour)}, "Todo", false},
		{"Done", nil, "Todo", false},
		{"Done", []time.Time{now.Add(-time.Hour)}, "Todo", true},
		{"Todo", []time.Time{now.Add(-time.Hour)}, "Todo", false},
		{"Done", []time.Time{now.Add(time.Hour)}, "Todo", false},
		{"Done", []time.Time{now.Add(-time.Hour)}, "Cancelled", false},
		{"Done", []time.Time{now.Add(-15 * 24 * time.Hour), now.Add(-time.Hour)}, "Todo", false},
		{"Cancelled", []time.Time{now.Add(-2 * time.Hour)}, "Todo", false},
		{"Cancelled", []time.Time{now.Add(-3 * 24 * time.Hour)}, "Todo", false},
	} {
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_window_%d", index)), Number: index + 1, Revision: 1, Profile: "native"}, Title: fmt.Sprintf("Window %d", index), Body: nativeContractTestBody, State: seed.state, Terminal: seed.state != "Todo", Archived: seed.archived, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: now.Add(-30 * 24 * time.Hour), UpdatedAt: now, LastActivityAt: now}
		to := seed.state
		if to == "Todo" {
			to = "Done"
		}
		var history []tracker.CollaborationEvent
		for _, at := range seed.entries {
			history = append(history, tracker.CollaborationEvent{Type: "workflow.transitioned", RecordedAt: at, Data: tracker.CollaborationData{Revision: 1, FromState: seed.from, ToState: to}})
		}
		if issue.Terminal && seed.from == "Todo" && len(seed.entries) > 0 {
			issue.ClosedAt = &seed.entries[len(seed.entries)-1]
		}
		issues, histories = append(issues, issue), append(histories, history)
	}
	importNativeWorkPageIssues(t, f, issues, histories)
	drainTestNativeBoard(t, f.service)
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authentication=%d %v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	windows := []tracker.NativeShippedWindow{{Window: "1h", Shipped: 3}, {Window: "24h", Shipped: 3, Cancelled: 1}, {Window: "7d", Shipped: 5, Cancelled: 2}}
	for _, test := range []struct {
		window, filter     string
		shipped, cancelled int
		status             int
	}{
		{"", "", 3, 1, http.StatusOK},
		{"1h", "", 3, 0, http.StatusOK},
		{"24h", "", 3, 1, http.StatusOK},
		{"7d", "", 5, 2, http.StatusOK},
		{"48h", "", 3, 1, http.StatusOK},
		{"14d", "", 3, 1, http.StatusOK},
		{"all", "", 3, 1, http.StatusOK},
		{"7d", "&state=Cancelled", 0, 2, http.StatusOK},
		{"24h", "&state=Todo", 0, 0, http.StatusOK},
		{"24h", "&archived=true", 3, 1, http.StatusOK},
		{"bad", "", 0, 0, http.StatusUnprocessableEntity},
		{"24h", "&completed_window=all", 0, 0, http.StatusUnprocessableEntity},
	} {
		t.Run(test.window+test.filter, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=work&completed_window="+test.window+test.filter, f.token, nil)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			var page tracker.NativeIssuePage
			decodeHubResponse(t, response, &page)
			params, err := url.ParseQuery("include=work&completed_window=" + test.window + test.filter)
			if err != nil {
				t.Fatal(err)
			}
			page, err = f.service.readBoardRecordsFor(t.Context(), scope, params, true)
			if err != nil {
				t.Fatal(err)
			}
			if page.Work == nil || page.Work.Completed != test.shipped || page.Work.Cancelled != test.cancelled {
				t.Fatalf("summary = %+v, want %d shipped and %d cancelled", page.Work, test.shipped, test.cancelled)
			}
			if test.filter == "" && !slices.Equal(page.Work.ShippedWindows, windows) {
				t.Fatalf("shipped windows = %+v, want %+v", page.Work.ShippedWindows, windows)
			}
		})
	}
}

func TestNativeWorkClosedOrdering(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authentication=%d %v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	project, err := f.service.readProjectRecord(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	project.States = append(slices.Clone(project.States), tracker.NativeState{Name: "Cancelled", Terminal: true})
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var expected []tracker.NativeWorkItemID
	var issues []tracker.NativeIssue
	var histories [][]tracker.CollaborationEvent
	for index, state := range []string{"Done", "Cancelled", "Done", "Done", "Done", "Todo"} {
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_closed_%d", index)), Number: index + 1, Revision: 1, Profile: "native"}, Title: fmt.Sprintf("Closed %d", index), Body: nativeContractTestBody, State: state, Terminal: state != "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, CreatedAt: now.Add(-30 * 24 * time.Hour), UpdatedAt: now, LastActivityAt: now}
		var history []tracker.CollaborationEvent
		if index < 4 {
			at := now.Add(-time.Duration(index+1) * time.Hour)
			issue.ClosedAt = &at
			history = []tracker.CollaborationEvent{{Type: "workflow.transitioned", RecordedAt: at, Data: tracker.CollaborationData{Revision: 1, FromState: "Todo", ToState: state}}}
		}
		if issue.Terminal {
			expected = append(expected, issue.WorkItemID)
		}
		issues, histories = append(issues, issue), append(histories, history)
	}
	importNativeWorkPageIssues(t, f, issues, histories)
	drainTestNativeBoard(t, f.service)
	params := url.Values{"include": {"work"}, "open": {"false"}, "sort": {"closed"}, "limit": {"2"}}
	var actual []tracker.NativeWorkItemID
	for {
		page, err := f.service.readIssues(t.Context(), scope, params)
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range page.Items {
			actual = append(actual, issue.WorkItemID)
			if !issue.Terminal {
				t.Fatal("closed query returned an active item")
			}
			index := len(actual) - 1
			if index < 4 && (issue.ClosedAt == nil || !issue.ClosedAt.Equal(now.Add(-time.Duration(index+1)*time.Hour))) {
				t.Fatalf("closed timestamp = %v", issue.ClosedAt)
			}
			if index == 4 && issue.ClosedAt != nil {
				t.Fatal("missing terminal history invented a timestamp")
			}
		}
		if page.NextCursor == "" {
			break
		}
		params.Set("cursor", page.NextCursor)
	}
	if !slices.Equal(actual, expected) {
		t.Fatalf("closed order = %v, want %v", actual, expected)
	}
	params.Set("sort", "unknown")
	params.Del("cursor")
	if _, err := f.service.readIssues(t.Context(), scope, params); err == nil {
		t.Fatal("accepted unknown sort")
	}
}

func importNativeWorkPageIssues(t *testing.T, f nativeFixture, issues []tracker.NativeIssue, histories [][]tracker.CollaborationEvent, recordBase ...int64) {
	t.Helper()
	source := newCoreNeutralFixture(t, f.project.OrganizationID, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
	states := map[string]int{}
	for index, issue := range issues {
		stateID, exists := states[issue.State]
		if !exists {
			stateID = len(states) + 1
			states[issue.State] = stateID
			source.Add("workflow_states", map[string]any{"id": stateID, "project_id": f.project.ID, "detent_state": issue.State, "terminal": issue.Terminal})
		}
		actor := string(historyTestJSON(t, issue.Actor))
		id := int64(1000 + index)
		if len(recordBase) == 1 {
			id = recordBase[0] + int64(index)
		}
		var contract any
		if issue.IssueContract != nil {
			contract = string(historyTestJSON(t, issue.IssueContract))
		}
		source.Add("issues", map[string]any{"id": id, "native_id": issue.WorkItemID, "organization_id": issue.OrganizationID, "project_id": issue.ProjectID, "number": issue.Number, "revision": issue.Revision, "event_sequence": len(histories[index]) + 1, "native_source_key": nil, "title": issue.Title, "body": issue.Body, "issue_contract_json": contract, "report_context_json": nil, "actor_json": actor, "provenance_json": nil, "archived": issue.Archived, "created_at": hubapi.FormatHubTime(issue.CreatedAt), "updated_at": hubapi.FormatHubTime(issue.UpdatedAt), "native_created_at": hubapi.FormatHubTime(issue.CreatedAt), "native_updated_at": hubapi.FormatHubTime(issue.UpdatedAt), "last_activity_at": hubapi.FormatHubTime(issue.LastActivityAt), "workflow_state_id": stateID, "terminal_entered_at": nil, "labels_json": string(historyTestJSON(t, issue.Labels)), "assignees_json": string(historyTestJSON(t, issue.Assignees)), "author_login": issue.Actor.PrincipalID, "repository_id": nil, "dispatch_wait_since": nil})
		source.Add("collaboration_versions", map[string]any{"organization_id": issue.OrganizationID, "project_id": issue.ProjectID, "work_item_id": issue.WorkItemID, "record_id": issue.WorkItemID, "revision": issue.Revision, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
		source.Add("queue_entries", map[string]any{"id": id, "issue_id": id, "scope": "native", "state": issue.State, "rank": fmt.Sprintf("%04d", index), "priority_override": nil, "created_at": hubapi.FormatHubTime(issue.CreatedAt), "updated_at": hubapi.FormatHubTime(issue.UpdatedAt)})
		events := append([]tracker.CollaborationEvent{{Type: "issue.created", RecordedAt: issue.CreatedAt, Data: tracker.CollaborationData{Revision: issue.Revision}}}, histories[index]...)
		for eventIndex, event := range events {
			source.Add("collaboration_events", map[string]any{"id": fmt.Sprintf("event_%s_%d", issue.WorkItemID, eventIndex), "organization_id": issue.OrganizationID, "project_id": issue.ProjectID, "work_item_id": issue.WorkItemID, "sequence": eventIndex + 1, "type": event.Type, "schema_version": 1, "actor_json": actor, "data_json": string(historyTestJSON(t, event.Data)), "event_revision": event.Data.Revision, "recorded_at": hubapi.FormatHubTime(event.RecordedAt)})
		}
	}
	input := coreimport.CoreWorkItemImportSource{BundlePath: source.Write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
	if count, err := coreimport.ImportCoreWorkItems(t.Context(), f.service.database.postgres, input); err != nil || count != int64(len(issues)) {
		t.Fatalf("historical page import count=%d error=%v", count, err)
	}
}

func mergeCoreNeutralProjectSources(t *testing.T, sources ...*neutralimporttest.Fixture) *neutralimporttest.Fixture {
	t.Helper()
	merged := sources[0].Clone(t)
	shift := func(cell storagecatalog.Cell, offset int64) storagecatalog.Cell {
		value, err := strconv.ParseInt(string(cell.Data), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		cell.Data = []byte(strconv.FormatInt(value+offset, 10))
		return cell
	}
	for index, source := range sources[1:] {
		offset := int64(1000 * (index + 1))
		for table, rows := range source.Clone(t).Records {
			for _, row := range rows {
				switch table {
				case "repositories":
					if slices.ContainsFunc(merged.Records[table], func(current neutralimport.Row) bool { return slices.Equal(current.Key, row.Key) }) {
						continue
					}
				case "workflow_states":
					row.Values["id"] = shift(row.Values["id"], offset)
					row.Key = []string{string(row.Values["id"].Data)}
					row.SourceRowID += offset
				case "issues":
					if !row.Values["workflow_state_id"].Null {
						row.Values["workflow_state_id"] = shift(row.Values["workflow_state_id"], offset)
					}
				}
				merged.Records[table] = append(merged.Records[table], row)
			}
		}
	}
	for table, rows := range merged.Records {
		key := merged.Tables[table].Key
		if len(key) == 1 && key[0].Column == "id" && key[0].Kind == "integer" {
			continue
		}
		for index := range rows {
			rows[index].SourceRowID = int64(index + 1)
		}
	}
	return merged
}
