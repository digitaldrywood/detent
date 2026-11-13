package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func updateOperatorSummaryFixtureIssue(t *testing.T, service *Service, scope hubscope.NativeScope, id tracker.NativeWorkItemID, update func(*tracker.NativeIssue)) tracker.NativeIssue {
	t.Helper()
	row, version, err := service.readWorkItemRecord(t.Context(), scope, string(id))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := service.hydrateCanonicalWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	update(&issue)
	issue.Revision++
	fields, err := service.prepareWorkItemFields(t.Context(), scope.Organization, hubrecords.WorkItemRecordPlan{Issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	version, err = eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: id, RecordID: string(id), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	row.Title, row.IssueType, row.Priority, row.Archived, row.LastActivityAt = issue.Title, issue.Type, issue.Priority, issue.Archived, issue.LastActivityAt
	row.BodyObjectKey, row.IssueContractObjectKey, row.ReportContextObjectKey = &fields.Body, fields.Contract, fields.Report
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if err := w.SetWorkItemLabels(t.Context(), scope.Project, id, issue.Labels); err != nil {
			return err
		}
		_, err := w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return issue
}

func appendOperatorSummaryFixtureTransition(t *testing.T, service *Service, scope hubscope.NativeScope, id tracker.NativeWorkItemID, data tracker.CollaborationData, at time.Time) {
	t.Helper()
	row, err := eventstore.NewContent(service.runDetailStore()).PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: id, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-owner"}, RecordedAt: at, Data: data}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendEvent(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

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
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
			issues := seedArchiveIssues(t, f.service, scope, 2, "Todo")
			large := strings.Repeat("<界", operatortool.WorkListPageBytes/4)
			switch test.field {
			case "related":
				if test.links > 0 {
					related := seedArchiveIssues(t, f.service, scope, test.links, "Todo")
					for _, target := range related {
						updateOperatorSummaryFixtureIssue(t, f.service, scope, target.WorkItemID, func(issue *tracker.NativeIssue) { issue.Archived = true })
						if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
							return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issues[0].WorkItemID, RelatedProjectID: scope.Project, RelatedWorkItemID: target.WorkItemID, Type: "related"}, "add", f.service.config.now())
						}); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					updateOperatorSummaryFixtureIssue(t, f.service, scope, issues[1].WorkItemID, func(issue *tracker.NativeIssue) { issue.Title = large })
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issues[0].WorkItemID, RelatedProjectID: scope.Project, RelatedWorkItemID: issues[1].WorkItemID, Type: "related"}, "add", f.service.config.now())
					}); err != nil {
						t.Fatal(err)
					}
				}
			case "latest_transition_reason":
				appendOperatorSummaryFixtureTransition(t, f.service, scope, issues[0].WorkItemID, tracker.CollaborationData{ReasonDetail: large}, f.service.config.now())
			}
			updateOperatorSummaryFixtureIssue(t, f.service, scope, issues[0].WorkItemID, func(issue *tracker.NativeIssue) {
				switch test.field {
				case "title":
					issue.Title = large
				case "labels":
					issue.Labels = []string{large}
				case "issue_contract":
					issue.IssueContract = &issuecontract.State{HumanAction: large}
				default:
					issue.Title = "Example item"
				}
			})
			for i, issue := range issues {
				updateOperatorSummaryFixtureIssue(t, f.service, scope, issue.WorkItemID, func(item *tracker.NativeIssue) { item.Priority = new(i) })
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					entry, err := w.QueueEntry(t.Context(), scope.Project, issue.WorkItemID, string(scope.Project))
					if err != nil {
						return err
					}
					entry.Priority = new(int64(i))
					return w.PutQueueEntry(t.Context(), entry)
				}); err != nil {
					t.Fatal(err)
				}
			}
			drainTestNativeBoard(t, f.service)
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			call := hostedContextProtocol(t, f.service, ctx, "http")
			args := map[string]any{"project_id": string(f.project.ID), "limit": 1}
			if len(test.include) > 0 {
				args["include"] = test.include
			}
			counts := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/board/counts", f.token, nil)
			requireNativeStatus(t, counts, http.StatusOK)
			var summary viewstore.BoardSummaryPage
			decodeHubResponse(t, counts, &summary)
			var counted int64
			for _, row := range summary.Rows {
				if row.Kind == "lane" {
					counted += row.Total
				}
			}
			if summary.ProjectionPending || counted != int64(len(issues)) {
				t.Fatalf("board counts total=%d pending=%t want=%d", counted, summary.ProjectionPending, len(issues))
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
				if envelope.Data.Total != -1 {
					t.Fatalf("bounded page total=%d want the uncounted sentinel -1", envelope.Data.Total)
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
	now := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return now }})
	short := tracker.ProjectID("p")
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Core.ID, project.Core.Name, project.Core.Slug = short, "Example", ""
		project.Settings.OrganizationID, project.Settings.ProjectID = f.project.OrganizationID, short
		if _, err := w.CreateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: credential.ID, ProjectID: short})
	}); err != nil {
		t.Fatal(err)
	}
	reasons := map[tracker.ProjectID]map[int]string{}
	for _, project := range []tracker.ProjectID{short, f.project.ID} {
		reasons[project] = map[int]string{}
		scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: project}
		issues := seedArchiveIssues(t, f.service, scope, 40, "Todo")
		for i, issue := range issues {
			updateOperatorSummaryFixtureIssue(t, f.service, scope, issue.WorkItemID, func(issue *tracker.NativeIssue) {
				issue.Title, issue.Type, issue.LastActivityAt = fmt.Sprintf("Fix %d", i), "bug", now
			})
			for _, reason := range []string{"old", "ready"} {
				if i == 1 {
					continue
				}
				if i == 0 {
					reason = ""
				}
				appendOperatorSummaryFixtureTransition(t, f.service, scope, issue.WorkItemID, tracker.CollaborationData{FromState: "Todo", ToState: "Todo", Reason: "operator", ReasonDetail: reason}, now)
				reasons[project][issue.Number] = reason
				if reason == "" {
					reasons[project][issue.Number] = "operator"
				}
			}
		}
	}
	drainTestNativeBoard(t, f.service)
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
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: short}
	for _, id := range []tracker.NativeWorkItemID{first.Data.Items[0].WorkItemID, tracker.NativeWorkItemID("")} {
		if id == "" {
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItemByNumber(t.Context(), short, int64(next.Data.Items[0].Number))
				id = item.NativeID
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		appendOperatorSummaryFixtureTransition(t, f.service, scope, id, tracker.CollaborationData{ReasonDetail: strings.Repeat("<", 8000)}, now.Add(time.Hour))
	}
	drainTestNativeBoard(t, f.service)
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
