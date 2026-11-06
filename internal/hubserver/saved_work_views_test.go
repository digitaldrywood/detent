package hubserver

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSavedWorkViews(t *testing.T) {
	t.Parallel()
	f := newPostgresWorkViewFixture(t)
	alice := f.user(t, "alice", "viewer", "alice@example.test", "read", "")
	bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
	base := "/api/v2/organizations/org_security/work-views"
	state := savedWorkState{View: "list", Tab: "closed", Q: "a search", Sort: "updated", CompletedWindow: "7d", CompletedFrom: "2026-10-26T11:00:00Z", CompletedTo: "2026-10-27T11:00:00Z", Archived: true, CreatedBy: "me", State: []string{"Done"}, Type: []string{"bug"}, Label: []string{"label-a", "label-b"}, Assignee: []string{"alice", "bob"}, Priority: []string{"1", "2"}, Lanes: []string{}, Collapsed: []string{"Todo", "Backlog"}}
	create := func(user hostedSecurityUser, name, scope string, project *string) savedWorkView {
		t.Helper()
		ids := []string{}
		if project != nil {
			ids = append(ids, *project)
		}
		response := f.request(t, user, http.MethodPost, base, savedWorkView{ProjectIDs: ids, Name: name, Scope: scope, ProjectID: project, Favorite: true, State: state})
		requireNativeStatus(t, response, http.StatusOK)
		var v savedWorkView
		decodeHubResponse(t, response, &v)
		if !v.CanEdit || !reflect.DeepEqual(v.State, state) || v.OwnerID != user.identity.Subject || !v.Favorite || v.Name != name {
			t.Fatalf("created view = %+v", v)
		}
		return v
	}
	list := func(user hostedSecurityUser) []savedWorkView {
		t.Helper()
		response := f.request(t, user, http.MethodGet, base, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			Views []savedWorkView `json:"views"`
		}
		decodeHubResponse(t, response, &result)
		return result.Views
	}
	personal := create(alice, "Personal", "personal", nil)
	other := create(bob, "Other", "personal", nil)
	if got := list(alice); len(got) != 1 || got[0].ID != personal.ID {
		t.Fatalf("personal visibility = %+v", got)
	}
	if got := list(bob); len(got) != 1 || got[0].ID != other.ID {
		t.Fatalf("other visibility = %+v", got)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		requireNativeStatus(t, f.request(t, bob, method, base+"/"+personal.ID, personal), http.StatusNotFound)
	}
	personal.Name = "Shared renamed"
	personal.Scope = "organization"
	response := f.request(t, alice, http.MethodPut, base+"/"+personal.ID, personal)
	requireNativeStatus(t, response, http.StatusOK)
	if got := list(bob); len(got) != 2 {
		t.Fatalf("shared list = %+v", got)
	}
	response = f.request(t, bob, http.MethodGet, base+"/"+personal.ID, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var shared savedWorkView
	decodeHubResponse(t, response, &shared)
	if shared.Favorite || shared.CanEdit || shared.Name != "Shared renamed" || !reflect.DeepEqual(shared.State, state) {
		t.Fatalf("shared view = %+v", shared)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		requireNativeStatus(t, f.request(t, bob, method, base+"/"+personal.ID, personal), http.StatusNotFound)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodPut, base+"/"+personal.ID+"/favorite", map[string]bool{"favorite": true}), http.StatusOK)
	requireNativeStatus(t, f.request(t, alice, http.MethodPut, base+"/"+personal.ID+"/favorite", map[string]bool{"favorite": false}), http.StatusOK)
	response = f.request(t, bob, http.MethodGet, base+"/"+personal.ID, nil)
	decodeHubResponse(t, response, &shared)
	if !shared.Favorite {
		t.Fatal("another user's favorite was removed")
	}
	project := string(f.project)
	projectView := create(alice, "Project", "organization", &project)
	if _, err := f.admin.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=$1", bob.identity.Subject); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+"/"+projectView.ID, nil), http.StatusNotFound)
	if got := list(bob); len(got) != 2 {
		t.Fatalf("grantless shared list = %+v", got)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodPost, base, savedWorkView{ProjectIDs: []string{project}, Name: "Denied", Scope: "personal", ProjectID: &project, State: state}), http.StatusForbidden)
	otherRepository := projectRecordPostgres(t)
	provider := newHostedSecurityProvider()
	second := hostedSecurityFixture{provider: provider, service: openTestService(t, Config{
		DatabasePath: hostedTestDatabasePath(t), GitHubDisabled: true,
		TenantRepository: otherRepository, AccessRepository: otherRepository,
		Hosted: &HostedConfig{OrganizationID: "org_other", WorkOSOrganizationID: "org_provider", PublicURL: "http://127.0.0.1:7777", Provider: provider},
	})}
	if err := otherRepository.WriteTenant(t.Context(), "org_other", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_other", Name: "Other organization", CreatedAt: time.Now()}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_other", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	charlie := second.user(t, "charlie", "member", "charlie@example.test", "", "")
	secondBase := "/api/v2/organizations/org_other/work-views"
	response = second.request(t, charlie, http.MethodPost, secondBase, savedWorkView{ProjectIDs: []string{}, Name: "Other organization", Scope: "organization", State: state})
	requireNativeStatus(t, response, http.StatusOK)
	var secondView savedWorkView
	decodeHubResponse(t, response, &secondView)
	requireNativeStatus(t, second.request(t, charlie, http.MethodGet, secondBase+"/"+personal.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, f.request(t, alice, http.MethodGet, base+"/"+secondView.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, second.request(t, alice, http.MethodGet, secondBase, nil), http.StatusForbidden)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		requireNativeStatus(t, f.request(t, alice, method, strings.Replace(base, "org_security", "org_other", 1)+"/"+personal.ID, personal), http.StatusNotFound)
	}
	personal.State.Lanes = nil
	personal.State.Label = []string{}
	personal.Scope = "personal"
	response = f.request(t, alice, http.MethodPut, base+"/"+personal.ID, personal)
	requireNativeStatus(t, response, http.StatusOK)
	var updated savedWorkView
	decodeHubResponse(t, response, &updated)
	if updated.State.Lanes != nil || len(updated.State.Label) != 0 {
		t.Fatalf("update retained old values: %+v", updated.State)
	}
	requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+"/"+personal.ID, nil), http.StatusNotFound)
	requireNativeStatus(t, f.request(t, alice, http.MethodDelete, base+"/"+personal.ID, nil), http.StatusNoContent)
	requireNativeStatus(t, f.request(t, alice, http.MethodGet, base+"/"+personal.ID, nil), http.StatusNotFound)
	if got := list(alice); len(got) != 1 || got[0].ID != projectView.ID {
		t.Fatalf("delete affected another view: %+v", got)
	}
	for _, query := range []string{"SELECT count(*) FROM hosted_work_view_values WHERE view_id=$1", "SELECT count(*) FROM hosted_work_view_favorites WHERE view_id=$1"} {
		var count int
		if err := f.admin.QueryRowContext(t.Context(), query, personal.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("delete %s: count=%d err=%v", query, count, err)
		}
	}
}

func TestSavedWorkViewValidation(t *testing.T) {
	t.Parallel()
	f := newPostgresWorkViewFixture(t)
	user := f.user(t, "alice", "member", "alice@example.test", "read", "")
	for _, test := range []struct {
		name   string
		change func(*savedWorkView)
	}{
		{"empty name", func(v *savedWorkView) { v.Name = " " }},
		{"long name", func(v *savedWorkView) { v.Name = strings.Repeat("x", 121) }},
		{"missing working set", func(v *savedWorkView) { v.ProjectIDs = nil }},
		{"duplicate project", func(v *savedWorkView) { v.ProjectIDs = []string{"one", "one"} }},
		{"empty project", func(v *savedWorkView) { v.ProjectIDs = []string{""} }},
		{"focus outside working set", func(v *savedWorkView) { v.ProjectID = new("outside") }},
		{"oversized working set", func(v *savedWorkView) { v.ProjectIDs = make([]string, 101) }},
		{"invalid scope", func(v *savedWorkView) { v.Scope = "public" }},
		{"invalid layout", func(v *savedWorkView) { v.State.View = "grid" }},
		{"invalid tab", func(v *savedWorkView) { v.State.Tab = "other" }},
		{"invalid sort", func(v *savedWorkView) { v.State.Sort = "other" }},
		{"invalid window", func(v *savedWorkView) { v.State.CompletedWindow = "forever" }},
		{"invalid completed from", func(v *savedWorkView) { v.State.CompletedFrom = "invalid" }},
		{"invalid completed to", func(v *savedWorkView) { v.State.CompletedTo = "invalid" }},
		{"invalid author", func(v *savedWorkView) { v.State.CreatedBy = "other" }},
		{"oversized search", func(v *savedWorkView) { v.State.Q = strings.Repeat("x", 8193) }},
		{"oversized selection", func(v *savedWorkView) { v.State.Label = make([]string, 257) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := savedWorkView{ProjectIDs: []string{}, Name: "View", Scope: "personal", State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
			test.change(&v)
			requireNativeStatus(t, f.request(t, user, http.MethodPost, "/api/v2/organizations/org_security/work-views", v), http.StatusUnprocessableEntity)
		})
	}
}

func TestSavedWorkViewBounds(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		large bool
	}{{"count page", false}, {"byte page", true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newPostgresWorkViewFixture(t)
			alice := f.user(t, "alice", "member", "alice@example.test", "read", "")
			bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
			base := "/api/v2/organizations/org_security/work-views"
			view := savedWorkView{ProjectIDs: []string{}, Name: "View", Scope: "organization", State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
			if test.large {
				view.State.Label = make([]string, 24)
				for i := range view.State.Label {
					view.State.Label[i] = strings.Repeat("x", 512)
				}
			}
			want := map[string]bool{}
			for range 26 {
				response := f.request(t, alice, http.MethodPost, base, view)
				requireNativeStatus(t, response, http.StatusOK)
				var created savedWorkView
				decodeHubResponse(t, response, &created)
				want[created.ID] = true
			}
			seen := map[string]bool{}
			cursor := ""
			for pageNumber := range 27 {
				response := f.request(t, bob, http.MethodGet, base+cursor, nil)
				requireNativeStatus(t, response, http.StatusOK)
				if response.Body.Len() > 256<<10 {
					t.Fatalf("page is %d bytes", response.Body.Len())
				}
				var page struct {
					Views      []savedWorkView `json:"views"`
					NextCursor string          `json:"next_cursor"`
				}
				decodeHubResponse(t, response, &page)
				if len(page.Views) == 0 || len(page.Views) > 25 {
					t.Fatalf("page has %d views", len(page.Views))
				}
				if pageNumber == 0 && (page.NextCursor == "" || (test.large && len(page.Views) >= 25)) {
					t.Fatalf("page was not bounded: views=%d cursor=%q", len(page.Views), page.NextCursor)
				}
				for _, got := range page.Views {
					if !want[got.ID] || seen[got.ID] {
						t.Fatalf("unexpected or duplicate view %s", got.ID)
					}
					labels := view.State.Label
					if labels == nil {
						labels = []string{}
					}
					if got.CanEdit || !reflect.DeepEqual(got.State.Label, labels) {
						t.Fatal("page changed ownership or selections")
					}
					seen[got.ID] = true
				}
				if page.NextCursor == "" {
					break
				}
				cursor = "?cursor=" + url.QueryEscape(page.NextCursor)
			}
			if len(seen) != len(want) {
				t.Fatalf("read %d of %d views", len(seen), len(want))
			}
			for _, query := range []string{"?cursor=a&cursor=b", "?limit=1000", "?cursor=" + strings.Repeat("x", 101)} {
				requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+query, nil), http.StatusUnprocessableEntity)
			}
		})
	}
	t.Run("payload", func(t *testing.T) {
		t.Parallel()
		f := newPostgresWorkViewFixture(t)
		alice := f.user(t, "alice", "member", "alice@example.test", "read", "")
		for _, test := range []struct {
			name  string
			state savedWorkState
		}{
			{"selections", savedWorkState{Label: func() []string {
				values := make([]string, 256)
				for i := range values {
					values[i] = strings.Repeat("x", 512)
				}
				return values
			}()}},
			{"escaped search", savedWorkState{Q: strings.Repeat("\x01", 8192)}},
		} {
			t.Run(test.name, func(t *testing.T) {
				state := test.state
				state.View = "board"
				state.Tab = "active"
				state.Sort = "default"
				state.CompletedWindow = "24h"
				response := f.request(t, alice, http.MethodPost, "/api/v2/organizations/org_security/work-views", savedWorkView{ProjectIDs: []string{}, Name: "View", Scope: "organization", State: state})
				if response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("oversized view status=%d, want 422", response.Code)
				}
			})
		}
	})
}

func TestSavedWorkViewQuotas(t *testing.T) {
	t.Parallel()
	f := newPostgresWorkViewFixture(t)
	users := make([]hostedSecurityUser, 5)
	for i := range users {
		name := fmt.Sprintf("member-%d", i)
		users[i] = f.user(t, name, "member", name+"@example.test", "read", "")
	}
	base := "/api/v2/organizations/org_security/work-views"
	view := savedWorkView{ProjectIDs: []string{}, Name: "View", Scope: "organization", State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}}
	first := make([]savedWorkView, 4)
	for i := range 4 {
		for j := range 50 {
			response := f.request(t, users[i], http.MethodPost, base, view)
			requireNativeStatus(t, response, http.StatusOK)
			if j == 0 {
				decodeHubResponse(t, response, &first[i])
			}
		}
		requireNativeStatus(t, f.request(t, users[i], http.MethodPost, base, view), http.StatusUnprocessableEntity)
		requireNativeStatus(t, f.request(t, users[i], http.MethodGet, base+"/"+first[i].ID, nil), http.StatusOK)
	}
	requireNativeStatus(t, f.request(t, users[4], http.MethodPost, base, view), http.StatusUnprocessableEntity)
	first[1].Name = "Renamed at capacity"
	requireNativeStatus(t, f.request(t, users[1], http.MethodPut, base+"/"+first[1].ID, first[1]), http.StatusOK)
	requireNativeStatus(t, f.request(t, users[0], http.MethodDelete, base+"/"+first[0].ID, nil), http.StatusNoContent)
	start := make(chan struct{})
	statuses := make(chan int, 2)
	for _, user := range []hostedSecurityUser{users[0], users[4]} {
		go func() {
			<-start
			statuses <- f.request(t, user, http.MethodPost, base, view).Code
		}()
	}
	close(start)
	success := 0
	for range 2 {
		switch status := <-statuses; status {
		case http.StatusOK:
			success++
		case http.StatusConflict, http.StatusUnprocessableEntity:
		default:
			t.Fatalf("concurrent create status=%d", status)
		}
	}
	if success != 1 {
		t.Fatalf("last slot admitted %d creates", success)
	}
	requireNativeStatus(t, f.request(t, users[1], http.MethodDelete, base+"/"+first[1].ID, nil), http.StatusNoContent)
	requireNativeStatus(t, f.request(t, users[4], http.MethodPost, base, view), http.StatusOK)
	requireNativeStatus(t, f.request(t, users[2], http.MethodGet, base+"/"+first[2].ID, nil), http.StatusOK)
}

func TestSavedWorkViewWorkingSets(t *testing.T) {
	t.Parallel()
	f := newPostgresWorkViewFixture(t)
	alice := f.user(t, "alice", "member", "alice@example.test", "read", "")
	bob := f.user(t, "bob", "member", "bob@example.test", "read", "")
	project := string(f.project)
	second := "prj_second"
	if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: tracker.ProjectID(second), Name: "Second project", Slug: "second-project", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	secondFixture := f
	secondFixture.project = tracker.ProjectID(second)
	secondFixture.grant(t, alice, false, false)
	base := "/api/v2/organizations/org_security/work-views"
	siblingResponse := f.request(t, alice, http.MethodPost, base, savedWorkView{Name: "Sibling", Scope: "personal", ProjectIDs: []string{second}, ProjectID: &second, State: savedWorkState{View: "board", Tab: "active", Sort: "default", CompletedWindow: "24h"}})
	requireNativeStatus(t, siblingResponse, http.StatusOK)
	var sibling savedWorkView
	decodeHubResponse(t, siblingResponse, &sibling)
	for _, test := range []struct {
		name  string
		ids   []string
		focus *string
	}{
		{"all", []string{}, nil},
		{"single", []string{project}, &project},
		{"multi focused", []string{second, project}, &second},
		{"multi all", []string{project, second}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := savedWorkView{Name: test.name, Scope: "organization", ProjectIDs: test.ids, ProjectID: test.focus, Favorite: true, State: savedWorkState{View: "list", Tab: "all", Sort: "title", CompletedWindow: "7d", Label: []string{"bug"}, Lanes: []string{"Todo"}, Collapsed: []string{"Done"}}}
			response := f.request(t, alice, http.MethodPost, base, input)
			requireNativeStatus(t, response, http.StatusOK)
			var created savedWorkView
			decodeHubResponse(t, response, &created)
			if !slices.Equal(created.ProjectIDs, test.ids) || !reflect.DeepEqual(created.ProjectID, test.focus) {
				t.Fatalf("created scope=%+v", created)
			}
			response = f.request(t, alice, http.MethodGet, base+"/"+created.ID, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var read savedWorkView
			decodeHubResponse(t, response, &read)
			if !reflect.DeepEqual(read, created) {
				t.Fatalf("read=%+v want=%+v", read, created)
			}
			if len(test.ids) > 1 {
				requireNativeStatus(t, f.request(t, bob, http.MethodGet, base+"/"+created.ID, nil), http.StatusNotFound)
				requireNativeStatus(t, f.request(t, bob, http.MethodPost, base, input), http.StatusForbidden)
				response = f.request(t, bob, http.MethodGet, base, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page savedWorkViewsPage
				decodeHubResponse(t, response, &page)
				if len(page.Views) != 0 {
					t.Fatalf("inaccessible views listed: %+v", page.Views)
				}
			}
			created.ProjectIDs = []string{second}
			created.ProjectID = &second
			created.State.Sort = "updated"
			response = f.request(t, alice, http.MethodPut, base+"/"+created.ID, created)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &read)
			if !slices.Equal(read.ProjectIDs, []string{second}) || read.ProjectID == nil || *read.ProjectID != second || read.State.Sort != "updated" || !slices.Equal(read.State.Label, []string{"bug"}) || !read.Favorite {
				t.Fatalf("updated=%+v", read)
			}
			requireNativeStatus(t, f.request(t, alice, http.MethodDelete, base+"/"+created.ID, nil), http.StatusNoContent)
			requireNativeStatus(t, f.request(t, alice, http.MethodGet, base+"/"+created.ID, nil), http.StatusNotFound)
			var count int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_work_view_projects WHERE view_id=$1`, created.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("membership cleanup count=%d err=%v", count, err)
			}
			requireNativeStatus(t, f.request(t, alice, http.MethodGet, base+"/"+sibling.ID, nil), http.StatusOK)
		})
	}
}

type postgresWorkViewFixture struct {
	hostedSecurityFixture
	repository tenantstore.Repository
	admin      *sql.DB
}

func newPostgresWorkViewFixture(t *testing.T) postgresWorkViewFixture {
	t.Helper()
	repository, fixture, _, admin := newChangePolicyPostgresFixtureWithAdmin(t, 4)
	if err := fixture.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	fixture.service.config.SecretKeys = secretTestKeys(t, "1", "1")
	envelope, err := fixture.service.config.SecretKeys.Seal([]byte(strings.Repeat("k", 32)), accessstore.SecretAAD("org_security", "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	now := fixture.service.config.now()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: "org_security", Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: "org_security", Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	return postgresWorkViewFixture{hostedSecurityFixture: fixture, repository: repository, admin: admin}
}

func (f postgresWorkViewFixture) user(t *testing.T, name, role, email, grant, supportActor string) hostedSecurityUser {
	t.Helper()
	user := f.hostedSecurityFixture.user(t, name, role, email, "", supportActor)
	if grant != "" {
		f.grant(t, user, grant == "write", false)
	}
	return user
}

func (f postgresWorkViewFixture) grant(t *testing.T, user hostedSecurityUser, write, runner bool) {
	t.Helper()
	if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: member.UserID, ProjectID: f.project, CanWrite: write, ManageRunner: runner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
}
