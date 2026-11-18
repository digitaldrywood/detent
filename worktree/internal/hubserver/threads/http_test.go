package threads_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/testenv/searchtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTeamThreadScopeAccess(t *testing.T) {
	f := newTeamThreadNativeFixture(t, nil, "", "first")
	second := newTeamThreadNativeFixture(t, f.service, f.project.OrganizationID, "second")
	foreign := newTeamThreadNativeFixture(t, f.service, f.project.OrganizationID+"_second", "foreign")
	issue := f.create(t, "subject")
	general := createTestThread(t, f, "general", "")
	project := createTestThread(t, f, "project", "")
	subject := createTestThread(t, f, "issue", issue.WorkItemID)
	other := createTestThread(t, second, "project", "")
	foreignThread := createTestThread(t, foreign, "general", "")
	if subject.ProjectID != string(f.project.ID) {
		t.Fatalf("issue scope did not derive project: %+v", subject)
	}
	for _, tc := range []struct {
		name   string
		viewer nativeFixture
		target teamThread
		status int
	}{
		{"general shared", second, general, 200}, {"project member", f, project, 200}, {"issue member", f, subject, 200},
		{"other project hidden", second, project, 404}, {"other issue hidden", second, subject, 404}, {"second project member", second, other, 200},
		{"foreign organization", foreign, general, http.StatusUnauthorized}, {"foreign general hidden", f, foreignThread, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/threads/" + tc.target.ID
			if tc.target.ID == foreignThread.ID {
				base = threadBase(foreign) + "/" + tc.target.ID
			}
			r := performHubAPIRequest(t, f.service, http.MethodGet, base, tc.viewer.token, nil)
			requireNativeStatus(t, r, tc.status)
		})
	}
	for _, tc := range []struct {
		f    nativeFixture
		want int
	}{{f, 3}, {second, 2}, {foreign, 1}} {
		r := performHubAPIRequest(t, f.service, http.MethodGet, threadBase(tc.f), tc.f.token, nil)
		requireNativeStatus(t, r, 200)
		var page teamThreadPage
		decodeHubResponse(t, r, &page)
		if len(page.Threads) != tc.want {
			t.Fatalf("visible threads=%d want %d: %s", len(page.Threads), tc.want, r.Body.String())
		}
	}
	for _, input := range []teamThreadInput{{Scope: "general", ProjectID: string(f.project.ID), Title: "bad"}, {Scope: "project", Title: "bad"}, {Scope: "issue", IssueID: "wi_missing", Title: "bad"}} {
		r := performHubAPIRequest(t, f.service, http.MethodPost, threadBase(f), f.token, input)
		if r.Code != 422 && r.Code != 404 {
			t.Fatalf("invalid scope: %d %s", r.Code, r.Body.String())
		}
	}
}

func TestTeamThreadReadCursorAndIsolation(t *testing.T) {
	f := newTeamThreadNativeFixture(t, nil, "", "cursor")
	other := f.service.OperatorKey(t, f.project.ID).Token
	issue := f.create(t, "thread subject")
	th := createTestThread(t, f, "issue", issue.WorkItemID)
	base := threadBase(f)
	for i, kind := range []string{"note", "finding", "question", "decision"} {
		r := performHubAPIRequest(t, f.service, http.MethodPost, base+"/"+th.ID+"/posts", other, teamThreadInput{Kind: kind, Body: "thread-only-sentinel", Key: kind})
		requireNativeStatus(t, r, 201)
		var post teamPost
		decodeHubResponse(t, r, &post)
		if post.Sequence != int64(i+1) {
			t.Fatalf("sequence=%d", post.Sequence)
		}
	}
	read := func(token, suffix string) teamThreadRead {
		t.Helper()
		r := performHubAPIRequest(t, f.service, http.MethodGet, base+"/"+th.ID+suffix, token, nil)
		requireNativeStatus(t, r, 200)
		var out teamThreadRead
		decodeHubResponse(t, r, &out)
		return out
	}
	page := read(f.token, "?since=last_read&limit=2")
	if len(page.Posts) != 2 || page.Cursor != "2" || !page.HasMore {
		t.Fatalf("first page=%+v", page)
	}
	page = read(f.token, "?since=last_read")
	if len(page.Posts) != 2 || page.Posts[0].Sequence != 3 || page.HasMore {
		t.Fatalf("second page=%+v", page)
	}
	if got := read(f.token, "?since=last_read"); len(got.Posts) != 0 {
		t.Fatalf("seen posts=%+v", got)
	}
	if got := read(other, "?since=last_read"); len(got.Posts) != 4 {
		t.Fatalf("member cursor shared: %+v", got)
	}
	for _, status := range []string{"resolved", "open"} {
		r := performHubAPIRequest(t, f.service, http.MethodPatch, base+"/"+th.ID, f.token, teamThreadInput{Status: status})
		requireNativeStatus(t, r, 200)
		var out teamThread
		decodeHubResponse(t, r, &out)
		if out.Status != status {
			t.Fatal(out)
		}
	}
}

func TestTeamThreadPaginationAndReferences(t *testing.T) {
	f := newTeamThreadNativeFixture(t, nil, "", "paging")
	second := newTeamThreadNativeFixture(t, f.service, f.project.OrganizationID, "other")
	issue := f.create(t, "linked issue")
	originalClock := f.clock.Now
	now := originalClock().UTC().Truncate(time.Microsecond)
	f.clock.Set(now)
	t.Cleanup(func() { f.clock.Set(originalClock()) })
	first := createTestThread(t, f, "general", "")
	now = now.Add(time.Microsecond)
	f.clock.Set(now)
	middle := createTestThread(t, f, "project", "")
	now = now.Add(time.Microsecond)
	f.clock.Set(now)
	last := createTestThread(t, f, "issue", issue.WorkItemID)
	f.clock.Set(time.Now())
	cursor := ""
	for i, want := range []string{last.ID, middle.ID, first.ID} {
		r := performHubAPIRequest(t, f.service, http.MethodGet, threadBase(f)+"?limit=1&cursor="+cursor, f.token, nil)
		requireNativeStatus(t, r, 200)
		var page teamThreadPage
		decodeHubResponse(t, r, &page)
		if len(page.Threads) != 1 || page.Threads[0].ID != want {
			t.Fatalf("page %d=%+v", i, page)
		}
		cursor = page.NextCursor
	}
	if cursor != "" {
		t.Fatalf("terminal cursor=%s", cursor)
	}
	r := performHubAPIRequest(t, f.service, http.MethodPatch, threadBase(f)+"/"+last.ID, f.token, teamThreadInput{Status: "resolved"})
	requireNativeStatus(t, r, 200)
	for _, tc := range searchtest.ThreadFilters(string(f.project.ID), string(issue.WorkItemID), first.ID, middle.ID, last.ID) {
		t.Run(tc.Name, func(t *testing.T) {
			r := performHubAPIRequest(t, f.service, http.MethodGet, threadBase(f)+"?"+tc.Query, f.token, nil)
			requireNativeStatus(t, r, 200)
			var page teamThreadPage
			decodeHubResponse(t, r, &page)
			if len(page.Threads) != len(tc.Want) {
				t.Fatalf("filtered=%+v want %v", page, tc.Want)
			}
			for i, want := range tc.Want {
				if page.Threads[i].ID != want {
					t.Fatalf("thread %d=%s want %s", i, page.Threads[i].ID, want)
				}
			}
		})
	}
	ref := operatortool.ThreadReference{Kind: "issue", ID: string(issue.WorkItemID)}
	r = performHubAPIRequest(t, f.service, http.MethodPost, threadBase(f)+"/"+first.ID+"/posts", f.token, teamThreadInput{Kind: "finding", Body: "Linked finding", References: []operatortool.ThreadReference{ref}})
	requireNativeStatus(t, r, 201)

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{{"visible reference", f.token, 1}, {"hidden reference", second.token, 0}} {
		t.Run("single reader/"+tc.name, func(t *testing.T) {
			guard := teamThreadReadGuard{Repository: f.service.Repository}
			guarded, _, _ := threadHTTPWithDetails(t, guard, f.project.OrganizationID, f.clock, false, f.details)
			ctx := context.WithValue(t.Context(), teamThreadReadGuardKey{}, &atomic.Int32{})
			request := httptest.NewRequest(http.MethodGet, threadBase(f)+"/"+first.ID, nil).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+tc.token)
			response := httptest.NewRecorder()
			guarded.Echo.ServeHTTP(response, request)
			requireNativeStatus(t, response, 200)
			var read teamThreadRead
			decodeHubResponse(t, response, &read)
			if len(read.Posts) != 1 || len(read.Posts[0].References) != tc.want {
				t.Fatalf("reference access=%+v", read)
			}
		})
	}
	r = performHubAPIRequest(t, f.service, http.MethodPost, threadBase(f)+"/"+first.ID+"/posts", second.token, teamThreadInput{Kind: "note", Body: "Not authorized to link", References: []operatortool.ThreadReference{ref}})
	if r.Code != http.StatusNotFound {
		t.Errorf("unreadable reference status=%d, want404: %s", r.Code, r.Body.String())
	}
	r = performHubAPIRequest(t, f.service, http.MethodGet, threadBase(f)+"/"+first.ID, f.token, nil)
	requireNativeStatus(t, r, http.StatusOK)
	var unchanged teamThreadRead
	decodeHubResponse(t, r, &unchanged)
	if len(unchanged.Posts) != 1 || unchanged.Thread.PostCount != 1 {
		t.Fatalf("refused reference changed thread history: %+v", unchanged)
	}
}

func TestTeamThreadGeneralMembershipAndRetries(t *testing.T) {
	f := newHostedSecurityFixture(t)
	base := "/api/v2/organizations/org_security/threads"
	for _, tc := range []struct {
		role   string
		status int
	}{{"member", 201}, {"reporter", 201}, {"viewer", 403}} {
		t.Run(tc.role, func(t *testing.T) {
			user := f.user(t, tc.role, tc.role, tc.role+"@example.test", "", "")
			input := teamThreadInput{Scope: "general", Title: "Organization research", Key: "same-intent"}
			r := f.request(t, user, http.MethodPost, base, input)
			requireNativeStatus(t, r, tc.status)
			if tc.status != 201 {
				return
			}
			var first teamThread
			decodeHubResponse(t, r, &first)
			r = f.request(t, user, http.MethodPost, base, input)
			requireNativeStatus(t, r, 201)
			var replay teamThread
			decodeHubResponse(t, r, &replay)
			if replay.ID != first.ID {
				t.Fatal("create retry duplicated thread")
			}
			input.Title = "Changed intent"
			requireNativeStatus(t, f.request(t, user, http.MethodPost, base, input), 409)
			post := teamThreadInput{Kind: "note", Body: "Shared research", Key: "reply-intent"}
			for range 2 {
				requireNativeStatus(t, f.request(t, user, http.MethodPost, base+"/"+first.ID+"/posts", post), 201)
			}
			post.Body = "Changed reply"
			requireNativeStatus(t, f.request(t, user, http.MethodPost, base+"/"+first.ID+"/posts", post), 409)
			r = f.request(t, user, http.MethodGet, base+"/"+first.ID, nil)
			requireNativeStatus(t, r, 200)
			var read teamThreadRead
			decodeHubResponse(t, r, &read)
			if len(read.Posts) != 1 || read.Thread.PostCount != 1 {
				t.Fatalf("post retry duplicated content: %+v", read)
			}
			body := &countingReader{reader: strings.NewReader(`{"kind":"note","key":"oversized","body":"` + strings.Repeat("x", int(4*hubapi.MaxRequestBodyBytes)) + `"}`)}
			request := httptest.NewRequest(http.MethodPost, base+"/"+first.ID+"/posts", body)
			request.AddCookie(&http.Cookie{Name: hubauth.Cookie, Value: user.Token})

			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			f.service.Echo.ServeHTTP(response, request)
			if response.Code < 400 || response.Code >= 500 || int64(body.read) > hubapi.MaxRequestBodyBytes+64<<10 {
				t.Fatalf("oversized thread post status=%d read=%d, want client error within the body limit", response.Code, body.read)
			}
		})
	}
}

func TestTeamThreadUnreadRoleDowngrade(t *testing.T) {
	v := newVerificationFixture(t)

	viewer := v.users["reporter"]
	base := "/api/v2/organizations/org_security/threads"
	for _, filer := range []string{"reporter", "tester"} {
		issue := v.issue(t, filer, "Private report")
		r := v.request(t, v.users["member"], http.MethodPost, base, teamThreadInput{Scope: "issue", IssueID: string(issue.WorkItemID), Title: "Private thread"})
		requireNativeStatus(t, r, http.StatusCreated)
		var thread teamThread
		decodeHubResponse(t, r, &thread)
		requireNativeStatus(t, v.request(t, v.users["member"], http.MethodPost, base+"/"+thread.ID+"/posts", teamThreadInput{Kind: "note", Body: "Private preview"}), http.StatusCreated)
	}
	read := func() teamUnreadSummary {
		t.Helper()
		r := v.request(t, viewer, http.MethodGet, base+"/unread?project_id="+string(v.project), nil)
		requireNativeStatus(t, r, http.StatusOK)
		var summary teamUnreadSummary
		decodeHubResponse(t, r, &summary)
		return summary
	}
	for _, tc := range []struct {
		role string
		want int64
	}{{"member", 2}, {"reporter", 1}, {"viewer", 2}, {"reporter", 1}} {
		t.Run(tc.role, func(t *testing.T) {
			if err := v.service.Repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(writer tenantstore.Writer) error {
				member, err := writer.AccessMember(t.Context(), viewer.Member.Member.UserID)
				if err != nil {
					return err
				}
				member.Role = tc.role
				return writer.UpdateAccessMember(t.Context(), member)
			}); err != nil {
				t.Fatal(err)
			}
			v.service.SetProviderRole(viewer.Member.Member.UserID, tc.role)
			if summary := read(); !summary.Pending || summary.Count != nil {
				t.Errorf("role change reused broader cached count: %+v", summary)
			}
			if err := v.threads.MaintainUnread(t.Context()); err != nil {
				t.Fatal(err)
			}
			if summary := read(); summary.Pending || summary.Count == nil || *summary.Count != tc.want {
				t.Errorf("wrong visibility after role change: %+v, want %d", summary, tc.want)
			}
		})
	}
}

func TestTeamThreadAuthorizationRoutes(t *testing.T) {
	for _, role := range []string{"owner", "admin", "member", "reporter", "viewer", "removed", "ungranted", "read_only"} {
		t.Run(role, func(t *testing.T) {
			v := newVerificationFixture(t)

			owner := v.user(t, "thread-owner", "owner", "thread-owner@example.test", "write", "")
			viewer := v.users["reporter"]
			if role != "reporter" {
				memberRole := role
				if role == "removed" || role == "ungranted" || role == "read_only" {
					memberRole = "member"
				}
				viewer = v.user(t, "thread-viewer", memberRole, "thread-viewer@example.test", "", "")
				if role != "ungranted" {
					v.grant(t, viewer, role != "read_only", false)
				}
			}
			if role == "removed" {
				v.service.RevokeMember(t, viewer.Member)
			}
			base := "/api/v2/organizations/org_security/threads"
			type target struct {
				name   string
				thread teamThread
				input  teamThreadInput
			}
			var targets []target
			for _, name := range []string{"general", "project", "own", "other"} {
				input := teamThreadInput{Scope: name, Title: "sentinel-preview-" + name}
				if name == "project" {
					input.ProjectID = string(v.project)
				}
				if name == "own" || name == "other" {
					filer := "reporter"
					if name == "other" {
						filer = "tester"
					}
					issue := v.issue(t, filer, "parent-"+name)
					input.Scope, input.IssueID = "issue", string(issue.WorkItemID)
				}
				r := v.request(t, owner, http.MethodPost, base, input)
				requireNativeStatus(t, r, http.StatusCreated)
				var thread teamThread
				decodeHubResponse(t, r, &thread)
				requireNativeStatus(t, v.request(t, owner, http.MethodPost, base+"/"+thread.ID+"/posts", teamThreadInput{Kind: "finding", Body: "preview-" + name}), http.StatusCreated)
				targets = append(targets, target{name, thread, input})
			}
			if role == "reporter" {
				if err := v.service.Repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(reader tenantstore.Reader) error {
					for index, filer := range []string{"reporter", "tester"} {
						item, err := reader.WorkItem(t.Context(), v.project, tracker.NativeWorkItemID(targets[index+2].input.IssueID))
						if err != nil {
							return err
						}
						member, err := reader.AccessMember(t.Context(), v.users[filer].Member.Member.UserID)
						if err != nil {
							return err
						}
						if item.ActorKind == nil || *item.ActorKind != "human" || item.ActorPrincipalID == nil || *item.ActorPrincipalID != member.PrincipalID {
							t.Fatalf("thread fixture lost typed %s actor: %+v", filer, item)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			readable := func(name string) bool {
				return role != "removed" && (role != "ungranted" || name == "general") && (role != "reporter" || name != "other")
			}
			call := func(t *testing.T, transport, operation, method, path string, input teamThreadInput) (json.RawMessage, bool) {
				t.Helper()
				if transport == "http" {
					var body any
					if method != http.MethodGet {
						body = input
					}
					r := v.request(t, viewer, method, path, body)
					return r.Body.Bytes(), r.Code >= 200 && r.Code < 300
				}
				t.Fatal("unexpected transport")
				return nil, false
			}
			t.Run("unread", func(t *testing.T) {
				path := base + "/unread"
				if role != "ungranted" {
					path += "?project_id=" + string(v.project)
				}
				r := v.request(t, viewer, http.MethodGet, path, nil)
				if role == "removed" {
					if r.Code < 400 {
						t.Fatalf("removed member read unread count: %s", r.Body.String())
					}
					return
				}
				requireNativeStatus(t, r, http.StatusOK)
				if err := v.threads.MaintainUnread(t.Context()); err != nil {
					t.Fatal(err)
				}
				r = v.request(t, viewer, http.MethodGet, path, nil)
				requireNativeStatus(t, r, http.StatusOK)
				var summary teamUnreadSummary
				decodeHubResponse(t, r, &summary)
				want := int64(4)
				switch role {
				case "reporter":
					want = 3
				case "ungranted":
					want = 1
				}
				if summary.Pending || summary.Count == nil || *summary.Count != want {
					t.Fatalf("unread includes inaccessible threads: %+v, want %d", summary, want)
				}
				if role == "ungranted" {
					r = v.request(t, viewer, http.MethodGet, base+"/unread?project_id="+string(v.project), nil)
					if r.Code < 400 {
						t.Fatalf("ungranted project unread accessible: %s", r.Body.String())
					}
				}
			})
			for _, transport := range []string{"http"} {
				t.Run(transport, func(t *testing.T) {
					for _, search := range []string{"", "sentinel", "preview"} {
						t.Run("list_"+search, func(t *testing.T) {
							raw, ok := call(t, transport, "list_threads", http.MethodGet, base+"?search="+search, teamThreadInput{Search: search})
							if role == "removed" {
								if ok {
									t.Fatalf("removed member listed threads: %s", raw)
								}
								return
							}
							if !ok {
								t.Fatalf("list failed: %s", raw)
							}
							var page teamThreadPage
							if err := json.Unmarshal(raw, &page); err != nil {
								t.Fatal(err)
							}
							for _, target := range targets {
								found := false
								for _, thread := range page.Threads {
									found = found || thread.ID == target.thread.ID
									if thread.ID == target.thread.ID && thread.Preview != "preview-"+target.name {
										t.Errorf("missing readable preview: %+v", thread)
									}
								}
								if found != readable(target.name) {
									t.Errorf("%s visible=%t, want %t: %s", target.name, found, readable(target.name), raw)
								}
								if !readable(target.name) {
									for _, secret := range []string{target.thread.ID, target.thread.Title, target.input.IssueID, "preview-" + target.name} {
										if secret != "" && strings.Contains(string(raw), secret) {
											t.Errorf("list leaked %q", secret)
										}
									}
								}
							}
						})
					}
					t.Run("issue_filter", func(t *testing.T) {
						other := targets[3]
						raw, ok := call(t, transport, "list_threads", http.MethodGet, base+"?issue_id="+other.input.IssueID, teamThreadInput{IssueID: other.input.IssueID})
						if role == "removed" {
							if ok {
								t.Fatalf("removed member listed issue threads: %s", raw)
							}
							return
						}
						if !ok {
							t.Fatalf("issue filter failed: %s", raw)
						}
						var page teamThreadPage
						if err := json.Unmarshal(raw, &page); err != nil {
							t.Fatal(err)
						}
						if (len(page.Threads) > 0) != readable("other") {
							t.Errorf("issue filter bypassed visibility: %s", raw)
						}
					})
					if role == "reporter" {
						t.Run("pagination", func(t *testing.T) {
							cursor := ""
							seen := map[string]bool{}
							for range 20 {
								raw, ok := call(t, transport, "list_threads", http.MethodGet, base+"?limit=1&cursor="+cursor, teamThreadInput{Limit: 1, Cursor: cursor})
								if !ok {
									t.Fatalf("page failed: %s", raw)
								}
								var page teamThreadPage
								if err := json.Unmarshal(raw, &page); err != nil {
									t.Fatal(err)
								}
								for _, thread := range page.Threads {
									if seen[thread.ID] || thread.IssueID == targets[3].input.IssueID {
										t.Fatalf("page duplicated or exposed hidden thread: %s", raw)
									}
									seen[thread.ID] = true
								}
								cursor = page.NextCursor
								if cursor == "" {
									break
								}
							}
							for _, target := range targets[:3] {
								if !seen[target.thread.ID] {
									t.Errorf("hidden thread displaced readable page for %s", target.name)
								}
							}
						})
					}
					for _, target := range targets {
						for _, route := range []struct {
							operation, method, suffix string
							input                     teamThreadInput
						}{
							{"get_thread", http.MethodGet, "/" + target.thread.ID, teamThreadInput{ThreadID: target.thread.ID}},
							{"create_thread", http.MethodPost, "", target.input},
							{"post_to_thread", http.MethodPost, "/" + target.thread.ID + "/posts", teamThreadInput{ThreadID: target.thread.ID, Kind: "note", Body: "preview-" + target.name}},
							{"resolve_thread", http.MethodPatch, "/" + target.thread.ID, teamThreadInput{ThreadID: target.thread.ID, Status: "resolved"}},
						} {
							t.Run(target.name+"_"+route.operation, func(t *testing.T) {
								raw, ok := call(t, transport, route.operation, route.method, base+route.suffix, route.input)
								want := readable(target.name) && (route.operation == "get_thread" || role != "viewer" && (role != "read_only" || target.name == "general"))
								if ok != want {
									t.Fatalf("allowed=%t, want %t: %s", ok, want, raw)
								}
								if !want {
									for _, secret := range []string{target.thread.ID, target.thread.Title, target.input.IssueID, "preview-" + target.name} {
										if secret != "" && strings.Contains(string(raw), secret) {
											t.Errorf("denied route leaked %q", secret)
										}
									}
								} else if route.operation == "get_thread" {
									var read teamThreadRead
									if err := json.Unmarshal(raw, &read); err != nil {
										t.Fatal(err)
									}
									if len(read.Posts) == 0 || !strings.Contains(read.Posts[0].Body, "preview-"+target.name) || read.Thread.IssueID != target.input.IssueID {
										t.Errorf("readable thread lost content: %s", raw)
									}
									if target.input.IssueID != "" && (read.Thread.IssueNumber == 0 || len(read.Thread.References) != 1 || read.Thread.References[0].ID != target.input.IssueID) {
										t.Errorf("readable parent reference missing: %s", raw)
									}
								}
							})
						}
					}
				})
			}
		})
	}
}
