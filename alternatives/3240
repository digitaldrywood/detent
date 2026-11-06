package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkViewRecordsUseCurrentPostgresAuthority(t *testing.T) {
	repository := projectRecordPostgres(t)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.RunDetails = &memoryRunDetails{}
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
	})
	org := tracker.OrganizationID("org_security")
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", Slug: "example-org", CreatedAt: f.service.config.now()}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "org_provider"}); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: f.project, Name: "Ra", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "owner", "viewer", "owner@example.test", "", "")
	other := f.user(t, "other", "viewer", "other@example.test", "", "")
	for _, user := range []hostedSecurityUser{owner, other} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), user.identity.Subject)
			if err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: f.project}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: member.PrincipalID, ProjectID: f.project})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: f.project, NativeID: "wi_readable_example", Number: 42, Title: "Readable example", ActorKind: new("human"), ActorPrincipalID: new("owner"), WorkflowStateName: new("Todo")}); err != nil {
			return err
		}
		return w.UpdateProjectSlug(t.Context(), f.project, "example-project", "renamed-project")
	}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"example-project", "renamed-project"} {
		var resolved map[string]string
		response := f.request(t, other, http.MethodGet, "/api/v2/organizations/org_security/readable/"+slug+"/issues/42", nil)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &resolved)
		if resolved["project_id"] != string(f.project) || resolved["work_item_id"] != "wi_readable_example" {
			t.Fatalf("resolved page=%v", resolved)
		}
	}
	for _, link := range []string{"/api/v2/organizations/org_security/projects/" + string(f.project) + "/work-items/wi_readable_example", "/work/i/wi_readable_example/changes/change_example"} {
		canonical, err := f.service.canonicalBrowserLink(t.Context(), link)
		expected := strings.TrimRight(f.service.config.Hosted.PublicURL, "/") + "/example-org/renamed-project/issues/42"
		if strings.HasSuffix(link, "/changes/change_example") {
			expected += "/changes/change_example"
		}
		if err != nil || canonical != expected {
			t.Fatalf("canonical link=%q want=%q error=%v", canonical, expected, err)
		}
	}
	for _, test := range []struct{ path, expected string }{
		{"/projects/" + string(f.project), "/example-org/renamed-project"},
		{"/settings", "/example-org/settings"},
	} {
		canonical, err := f.service.canonicalBrowserLink(t.Context(), test.path)
		expected := strings.TrimRight(f.service.config.Hosted.PublicURL, "/") + test.expected
		if err != nil || canonical != expected {
			t.Fatalf("canonical %s=%q want=%q error=%v", test.path, canonical, expected, err)
		}
	}
	now := f.service.config.now()
	envelope, err := f.service.config.SecretKeys.Seal(bytes.Repeat([]byte{3}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
			return err
		}
		_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: f.project, NativeID: "wi_second_example", Number: 43, Title: "Second example", ActorKind: new("human"), ActorPrincipalID: new("owner"), WorkflowStateName: new("Todo")})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for index, id := range []tracker.NativeWorkItemID{"wi_readable_example", "wi_second_example"} {
		searchTitle := "Readable example"
		if index == 1 {
			searchTitle = "Second example"
		}
		searchText := searchstore.Text{Title: searchTitle}
		if index == 0 {
			searchText.Title += ":"
			searchText.Body = "-tail"
			searchText.Labels = []string{"東京語"}
		}
		document, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: f.project, WorkItemID: id, ID: "issue:" + string(id), SourceID: string(id), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: now}, searchText)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.PutSearch(t.Context(), document, 0) }); err != nil {
			t.Fatal(err)
		}
		card := tracker.NativeBoardCard{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: f.project, WorkItemID: id, Number: 42 + index, Revision: 1}, Title: "Example", State: "Todo", Actor: tracker.Actor{PrincipalID: "owner"}}}
		ref, err := viewstore.NewContent(f.service.runDetailStore()).PrepareBoard(t.Context(), card)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.Project(t.Context(), f.project)
			if err != nil {
				return err
			}
			record := viewstore.BoardRecord{OrganizationID: org, ProjectID: f.project, WorkItemID: id, SourceRevision: 1, SourceProjectRevision: project.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", ActivityMissing: true, Identifier: fmt.Sprintf("%s#%d", f.project, 42+index)}, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: ref}
			return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: f.project, WorkItemID: id, ExpectedRevision: 1, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &record})
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.service.outbox.stop()
	sqliteReader := f.service.database.reader
	f.service.database.reader = nil
	defer func() { f.service.database.reader = sqliteReader }()
	boardURL := f.base + "/work-items?include=board&limit=1"
	refreshSearch := func(t *testing.T) {
		t.Helper()
		for range 500 {
			more, err := searchstore.NewContent(f.service.runDetailStore()).RefreshOne(t.Context(), tenantSearchRepository{repository}, org)
			if err != nil {
				t.Fatal(err)
			}
			if !more {
				return
			}
		}
		t.Fatal("search fixture refresh did not finish")
	}
	requireNativeStatus(t, f.request(t, other, http.MethodGet, boardURL+"&q=example", nil), http.StatusServiceUnavailable)
	refreshSearch(t)
	for _, test := range []struct {
		query string
		want  []tracker.NativeWorkItemID
	}{
		{"readable", []tracker.NativeWorkItemID{"wi_readable_example"}},
		{"ead", []tracker.NativeWorkItemID{"wi_readable_example"}},
		{"東", []tracker.NativeWorkItemID{"wi_readable_example"}},
		{"東京語", []tracker.NativeWorkItemID{"wi_readable_example"}},
		{":", []tracker.NativeWorkItemID{"wi_readable_example"}},
		{":-", nil},
		{":-~", nil},
		{"\x00", nil},
		{"&", nil},
		{"nomatchword", nil},
		{"example", []tracker.NativeWorkItemID{"wi_readable_example", "wi_second_example"}},
	} {
		path := boardURL + "&q=" + url.QueryEscape(test.query)
		var ids []tracker.NativeWorkItemID
		for pageIndex := 0; ; pageIndex++ {
			if pageIndex >= 3 {
				t.Fatal("search cursor did not finish")
			}
			response := f.request(t, other, http.MethodGet, path, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("search %q response=%s", test.query, response.Body.String())
			}
			var result tracker.NativeIssuePage
			decodeHubResponse(t, response, &result)
			for _, card := range result.Cards {
				if !strings.Contains(strings.ToLower(card.Issue.Snippet), test.query) {
					t.Fatalf("search %q missing matching snippet: %q", test.query, card.Issue.Snippet)
				}
				ids = append(ids, card.Issue.WorkItemID)
			}
			if result.Total != len(test.want) {
				t.Fatalf("search %q total=%d want=%d", test.query, result.Total, len(test.want))
			}
			if result.NextCursor == "" {
				break
			}
			path = boardURL + "&q=" + url.QueryEscape(test.query) + "&cursor=" + url.QueryEscape(result.NextCursor)
		}
		if !slices.Equal(ids, test.want) {
			t.Fatalf("search %q items=%v want=%v", test.query, ids, test.want)
		}
	}
	longQuery := strings.Repeat("x", searchstore.MaxQueryTextBytes+1)
	for _, path := range []string{
		boardURL + "&q=" + url.QueryEscape(longQuery),
		f.base + "/work-items?q=" + url.QueryEscape(longQuery),
	} {
		requireNativeStatus(t, f.request(t, other, http.MethodGet, path, nil), http.StatusUnprocessableEntity)
	}
	var board tracker.NativeIssuePage
	boardResponse := f.request(t, other, http.MethodGet, boardURL, nil)
	requireNativeStatus(t, boardResponse, http.StatusOK)
	decodeHubResponse(t, boardResponse, &board)
	requireNativeStatus(t, f.request(t, other, http.MethodGet, boardURL+"&created_after=2999-01-01T00%3A00%3A00Z", nil), http.StatusUnprocessableEntity)
	if len(board.Cards) != 1 || board.Cards[0].Issue.WorkItemID != "wi_readable_example" || board.Total != 2 || board.NextCursor == "" {
		t.Fatalf("first board page=%+v", board)
	}
	t.Run("issue list keeps global count and search keyset across pages", func(t *testing.T) {
		read := func(path string) tracker.NativeIssuePage {
			t.Helper()
			response := f.request(t, other, http.MethodGet, path, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var result tracker.NativeIssuePage
			decodeHubResponse(t, response, &result)
			return result
		}
		path := f.base + "/work-items?q=example&limit=1"
		first := read(path)
		if first.Total != 2 || first.ChangeSequence == 0 || len(first.Items) != 1 || first.Items[0].WorkItemID != "wi_second_example" || first.NextCursor == "" {
			t.Fatalf("first issue list page=%+v", first)
		}
		second := read(path + "&cursor=" + url.QueryEscape(first.NextCursor))
		if second.Total != first.Total || len(second.Items) != 1 || second.Items[0].WorkItemID != "wi_readable_example" || second.NextCursor != "" {
			t.Fatalf("second issue list page=%+v", second)
		}
		work := read(f.base + "/work-items?include=work&q=example&limit=1")
		if work.Work == nil || work.Total != 2 || len(work.Work.Items) != 1 || work.Work.Items[0].WorkItemID != "wi_second_example" || !work.Work.Truncated {
			t.Fatalf("work issue list projection=%+v", work)
		}
		invalidPicker := f.request(t, other, http.MethodGet, f.base+"/work-items?fields=picker&include=summary", nil)
		requireNativeStatus(t, invalidPicker, http.StatusUnprocessableEntity)
		pickerPath := f.base + "/work-items?fields=picker&q=example&limit=1"
		pickerResponse := f.request(t, other, http.MethodGet, pickerPath, nil)
		requireNativeStatus(t, pickerResponse, http.StatusOK)
		var picker nativeIssuePickerPage
		decodeHubResponse(t, pickerResponse, &picker)
		if picker.Total != 2 || len(picker.Items) != 1 || picker.Items[0].WorkItemID != "wi_second_example" || picker.NextCursor == "" {
			t.Fatalf("picker issue list=%+v", picker)
		}
		continuedPicker := f.request(t, other, http.MethodGet, pickerPath+"&cursor="+url.QueryEscape(picker.NextCursor), nil)
		requireNativeStatus(t, continuedPicker, http.StatusOK)
		picker = nativeIssuePickerPage{}
		decodeHubResponse(t, continuedPicker, &picker)
		if picker.Total != 2 || len(picker.Items) != 1 || picker.Items[0].WorkItemID != "wi_readable_example" || picker.NextCursor != "" {
			t.Fatalf("continued picker issue list=%+v", picker)
		}
	})

	streamRequest := httptest.NewRequest(http.MethodGet, f.base+"/events?board=true", nil)
	streamRequest.AddCookie(&http.Cookie{Name: hostedCookie, Value: other.token})
	streamContext := f.service.echo.NewContext(streamRequest, httptest.NewRecorder())
	streamCredential, _, err := f.service.appCredential(streamContext)
	if err != nil {
		t.Fatal(err)
	}
	streamScope := nativeScope{organization: org, project: f.project, credential: streamCredential}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.AddAccessOrganizationSlug(t.Context(), "retained-org")
	}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"example-org", "retained-org"} {
		parts := []string{slug, "example-project", "issues", "42"}
		id, err := f.service.readableRecordReference(t.Context(), streamScope, parts, true)
		if err != nil || id != "wi_readable_example" {
			t.Fatalf("readable reference %s=%q error=%v", slug, id, err)
		}
		canonical, err := f.service.canonicalBrowserLink(t.Context(), "/"+strings.Join(parts, "/"))
		expected := strings.TrimRight(f.service.config.Hosted.PublicURL, "/") + "/example-org/renamed-project/issues/42"
		if err != nil || canonical != expected {
			t.Fatalf("retained canonical=%q want=%q error=%v", canonical, expected, err)
		}
	}
	if _, err := f.service.readableRecordReference(t.Context(), streamScope, []string{"foreign-org", "example-project", "issues", "42"}, true); !isNativeNotFound(err) {
		t.Fatalf("foreign organization reference=%v", err)
	}
	if err := f.service.requireBoardRecordProject(t.Context(), streamScope); err != nil {
		t.Fatal(err)
	}
	delivery := newDashboardDelivery(f.service)
	delivery.config.pageSize = 1
	subscription := &dashboardSubscription{request: streamRequest, scope: streamScope, board: true, initial: true, params: url.Values{}, frames: make(chan dashboardFrame, 4), done: make(chan struct{})}
	readFrame := func() tracker.NativeBoardFrame {
		t.Helper()
		select {
		case raw := <-subscription.frames:
			var frame tracker.NativeBoardFrame
			if err := json.Unmarshal([]byte(raw.data), &frame); err != nil {
				t.Fatal(err)
			}
			return frame
		default:
			t.Fatal("board stream did not enqueue a frame")
			return tracker.NativeBoardFrame{}
		}
	}
	for pageIndex := 0; pageIndex < 2; pageIndex++ {
		more, err := f.service.dashboardPage(t.Context(), delivery, subscription)
		if err != nil || more != (pageIndex == 0) {
			t.Fatalf("stream page %d: more=%v error=%v", pageIndex, more, err)
		}
		frame := readFrame()
		if frame.Gap || len(frame.Deltas) != 1 || frame.Sequence != int64(pageIndex+1) || frame.GrantVersion != board.GrantVersion || frame.Work == nil {
			t.Fatalf("stream frame=%+v", frame)
		}
	}

	grantVersion := board.GrantVersion
	if grantVersion == "" {
		t.Fatal("board has no access version")
	}
	nextResponse := f.request(t, other, http.MethodGet, boardURL+"&cursor="+url.QueryEscape(board.NextCursor), nil)
	requireNativeStatus(t, nextResponse, http.StatusOK)
	board = tracker.NativeIssuePage{}
	decodeHubResponse(t, nextResponse, &board)
	if len(board.Cards) != 1 || board.Cards[0].Issue.WorkItemID != "wi_second_example" || board.NextCursor != "" {
		t.Fatalf("continued board page=%+v", board)
	}
	wideResponse := f.request(t, other, http.MethodGet, f.base+"/work-items?include=board&limit=2000", nil)
	requireNativeStatus(t, wideResponse, http.StatusOK)
	board = tracker.NativeIssuePage{}
	decodeHubResponse(t, wideResponse, &board)
	if len(board.Cards) != 2 {
		t.Fatalf("wide board page=%+v", board)
	}

	t.Run("issue list preserves workspace, native date, and fingerprint filters", func(t *testing.T) {
		var workspaceRecord viewstore.BoardRecord
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			workspaceRecord, err = r.BoardItem(t.Context(), f.project, "wi_second_example")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		updateLabels := func(ctx context.Context, labels []string) error {
			return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.BoardItem(ctx, f.project, "wi_second_example")
				if err != nil {
					return err
				}
				card, err := viewstore.NewContent(f.service.runDetailStore()).BoardRecord(ctx, record)
				if err != nil {
					return err
				}
				card.Issue.Labels = slices.Clone(labels)
				ref, err := viewstore.NewContent(f.service.runDetailStore()).PrepareBoard(ctx, card)
				if err != nil {
					return err
				}
				record.Labels = slices.Clone(labels)
				record.Payload = ref
				return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: f.project, WorkItemID: record.WorkItemID, ExpectedRevision: record.SourceRevision, ExpectedEventSequence: record.SourceEventSequence, ExpectedProjectRevision: record.SourceProjectRevision, Current: &record})
			})
		}
		if err := updateLabels(t.Context(), []string{"detent:workspace"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := updateLabels(context.Background(), workspaceRecord.Labels); err != nil {
				t.Errorf("restore workspace fixture labels: %v", err)
			}
		})
		read := func(query string) tracker.NativeIssuePage {
			t.Helper()
			path := f.base + "/work-items?" + query
			response := f.request(t, other, http.MethodGet, path, nil)
			if response.Code != http.StatusOK {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: other.token})
				call := f.service.echo.NewContext(request, httptest.NewRecorder())
				credential, _, authErr := f.service.appCredential(call)
				params, parseErr := url.ParseQuery(query)
				_, readErr := f.service.readIssues(t.Context(), nativeScope{organization: org, project: f.project, credential: credential}, params)
				t.Fatalf("issue list status=%d auth=%v parse=%v read=%v response=%s", response.Code, authErr, parseErr, readErr, response.Body.String())
			}
			var result tracker.NativeIssuePage
			decodeHubResponse(t, response, &result)
			return result
		}
		ordinary := read("limit=10")
		workspace := read("include=workspace&limit=10")
		if ordinary.Total != 1 || len(ordinary.Items) != 1 || ordinary.Items[0].WorkItemID != "wi_readable_example" || workspace.Total != 2 {
			t.Fatalf("workspace list ordinary=%+v included=%+v", ordinary, workspace)
		}
		future := read("created_after=2999-01-01T00%3A00%3A00Z")
		futureWorkspace := read("include=workspace&created_after=2999-01-01T00%3A00%3A00Z")
		if future.Total != 0 || len(future.Items) != 0 || futureWorkspace.Total != 0 || len(futureWorkspace.Items) != 0 {
			t.Fatalf("future created_after returned ordinary=%+v workspace=%+v", future, futureWorkspace)
		}
		document, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: f.project, WorkItemID: "wi_readable_example", ID: "issue:wi_readable_example", SourceID: "wi_readable_example", Kind: "issue", Scope: "issue", Revision: 2, UpdatedAt: now}, searchstore.Text{Title: "Readable example", Body: "\nfingerprint: source-fingerprint-123\n", Labels: []string{"東京語"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.PutSearch(t.Context(), document, 1) }); err != nil {
			t.Fatal(err)
		}
		refreshSearch(t)
		fingerprint := read("fingerprint=source-fingerprint-123")
		if fingerprint.Total != 1 || len(fingerprint.Items) != 1 || fingerprint.Items[0].WorkItemID != "wi_readable_example" {
			t.Fatalf("fingerprint filter returned %+v", fingerprint)
		}
	})

	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: other.identity.Subject, ProjectID: f.project, CanWrite: true})
	}); err != nil {
		t.Fatal(err)
	}
	versionResponse := f.request(t, other, http.MethodGet, boardURL, nil)
	requireNativeStatus(t, versionResponse, http.StatusOK)
	board = tracker.NativeIssuePage{}
	decodeHubResponse(t, versionResponse, &board)
	if board.GrantVersion == "" || board.GrantVersion == grantVersion {
		t.Fatal("committed grant change did not invalidate board access version")
	}

	if more, err := f.service.dashboardPage(t.Context(), delivery, subscription); err != nil || more {
		t.Fatalf("changed grants stream: more=%v error=%v", more, err)
	}
	changedFrame := readFrame()
	if !changedFrame.Gap || len(changedFrame.Deltas) != 0 || changedFrame.GrantVersion != board.GrantVersion {
		t.Fatalf("changed grant frame=%+v", changedFrame)
	}
	for _, query := range []string{"archived=invalid", "open=invalid", "priority=invalid"} {
		requireNativeStatus(t, f.request(t, other, http.MethodGet, f.base+"/work-items?include=board&"+query, nil), http.StatusUnprocessableEntity)
	}
	f.service.database.reader = sqliteReader
	preferences := f.base + "/work-view-preference"
	missingPreference := f.request(t, owner, http.MethodGet, preferences, nil)
	requireNativeStatus(t, missingPreference, http.StatusOK)
	var emptyPreference workViewPreference
	decodeHubResponse(t, missingPreference, &emptyPreference)
	if emptyPreference.Query != nil {
		t.Fatal("unset preference returned a query")
	}
	response := f.request(t, owner, http.MethodPut, preferences, map[string]string{"query": "view=list"})
	requireNativeStatus(t, response, http.StatusOK)
	var preference workViewPreference
	decodeHubResponse(t, f.request(t, owner, http.MethodGet, preferences, nil), &preference)
	if preference.Query == nil || *preference.Query != "view=list" {
		t.Fatalf("saved preference=%+v", preference)
	}
	base := "/api/v2/organizations/org_security/work-views"
	v := savedWorkView{Name: "Example view", Scope: "organization", ProjectIDs: []string{string(f.project)}, Favorite: true, State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
	response = f.request(t, owner, http.MethodPost, base, v)
	requireNativeStatus(t, response, http.StatusOK)
	var created savedWorkView
	decodeHubResponse(t, response, &created)
	if !created.Favorite || !created.CanEdit || created.OwnerID != owner.identity.Subject {
		t.Fatalf("created view=%+v", created)
	}
	response = f.request(t, other, http.MethodGet, base+"/"+created.ID, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var shared savedWorkView
	decodeHubResponse(t, response, &shared)
	if shared.CanEdit || shared.Favorite {
		t.Fatalf("shared owner state leaked=%+v", shared)
	}
	requireNativeStatus(t, f.request(t, other, http.MethodDelete, base+"/"+created.ID, nil), http.StatusNotFound)
	var sqliteCount int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM hosted_work_views)+(SELECT count(*) FROM hosted_work_view_preferences)").Scan(&sqliteCount); err != nil || sqliteCount != 0 {
		t.Fatalf("SQLite view writes=%d error=%v", sqliteCount, err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), other.identity.Subject, f.project)
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, other, http.MethodGet, base+"/"+created.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, f.request(t, other, http.MethodGet, "/api/v2/organizations/org_security/readable/example-project/issues/42", nil), http.StatusNotFound)
	workingSet, err := json.Marshal(savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"})
	if err != nil {
		t.Fatal(err)
	}
	workingSetURL := "/api/v2/organizations/org_security/work-board?" + url.Values{"project": {string(f.project)}, "state": {string(workingSet)}}.Encode()
	f.service.database.reader = nil
	deniedWorkingSet := f.request(t, other, http.MethodGet, workingSetURL, nil)
	f.service.database.reader = sqliteReader
	if deniedWorkingSet.Code != http.StatusForbidden && deniedWorkingSet.Code != http.StatusNotFound {
		t.Fatalf("revoked working-set access status=%d", deniedWorkingSet.Code)
	}
	if _, err := f.service.dashboardPage(t.Context(), delivery, subscription); err == nil {
		t.Fatal("revoked subscription retained project access")
	}
	deniedBoard := f.request(t, other, http.MethodGet, boardURL, nil)
	if deniedBoard.Code != http.StatusForbidden && deniedBoard.Code != http.StatusNotFound {
		t.Fatalf("revoked board access status=%d", deniedBoard.Code)
	}
	t.Run("search totals span candidate pages", func(t *testing.T) {
		const count = 201
		for index := 0; index < count; index++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_window_%03d", index))
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: f.project, WorkItemID: id, Number: 1000 + index, Revision: 1}, Title: "Windowmatch", State: "Todo", Actor: tracker.Actor{PrincipalID: "owner"}}
			ref, err := viewstore.NewContent(f.service.runDetailStore()).PrepareBoard(t.Context(), tracker.NativeBoardCard{Issue: issue})
			if err != nil {
				t.Fatal(err)
			}
			document, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: f.project, WorkItemID: id, ID: "issue:" + string(id), SourceID: string(id), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: now}, searchstore.Text{Title: issue.Title})
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: f.project, NativeID: id, Number: int64(issue.Number), Title: issue.Title, ActorKind: new("human"), ActorPrincipalID: new("owner"), WorkflowStateName: new("Todo")}); err != nil {
					return err
				}
				if err := w.PutSearch(t.Context(), document, 0); err != nil {
					return err
				}
				project, err := w.Project(t.Context(), f.project)
				if err != nil {
					return err
				}
				record := viewstore.BoardRecord{OrganizationID: org, ProjectID: f.project, WorkItemID: id, SourceRevision: 1, SourceProjectRevision: project.ContentSettingsRevision, Position: viewstore.BoardPosition{State: "Todo", ActivityMissing: true, Identifier: fmt.Sprintf("%s#%d", f.project, issue.Number)}, ActorPrincipalID: "owner", CreatedAt: now, UpdatedAt: now, Payload: ref}
				return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: f.project, WorkItemID: id, ExpectedRevision: 1, ExpectedProjectRevision: project.ContentSettingsRevision, Current: &record})
			}); err != nil {
				t.Fatal(err)
			}
		}
		refreshSearch(t)
		response := f.request(t, owner, http.MethodGet, boardURL+"&q=windowmatch", nil)
		requireNativeStatus(t, response, http.StatusOK)
		var result tracker.NativeIssuePage
		decodeHubResponse(t, response, &result)
		if result.Total != count {
			t.Fatalf("search total=%d want=%d across candidate pages", result.Total, count)
		}
		if len(result.Cards) != 1 || result.Cards[0].Issue.WorkItemID != "wi_window_000" {
			t.Fatalf("search did not preserve global board order: %+v", result.Cards)
		}
		listPath := f.base + "/work-items?q=windowmatch&limit=1"
		first := f.request(t, owner, http.MethodGet, listPath, nil)
		requireNativeStatus(t, first, http.StatusOK)
		var cursorPage tracker.NativeIssuePage
		decodeHubResponse(t, first, &cursorPage)
		if cursorPage.NextCursor == "" {
			t.Fatal("large issue list did not return a cursor")
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.BoardItem(t.Context(), f.project, "wi_window_000")
			if err != nil {
				return err
			}
			record.Position.Lane++
			return w.ReplaceBoardItem(t.Context(), viewstore.BoardUpdate{ProjectID: f.project, WorkItemID: record.WorkItemID, ExpectedRevision: record.SourceRevision, ExpectedEventSequence: record.SourceEventSequence, ExpectedProjectRevision: record.SourceProjectRevision, Current: &record})
		}); err != nil {
			t.Fatal(err)
		}
		stale := f.request(t, owner, http.MethodGet, listPath+"&cursor="+url.QueryEscape(cursorPage.NextCursor), nil)
		requireNativeStatus(t, stale, http.StatusUnprocessableEntity)
	})

}
