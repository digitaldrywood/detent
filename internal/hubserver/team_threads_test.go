package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func threadBase(f nativeFixture) string {
	return "/api/v2/organizations/" + string(f.project.OrganizationID) + "/threads"
}

func createTestThread(t *testing.T, f nativeFixture, scope string, issue tracker.NativeWorkItemID) teamThread {
	t.Helper()
	in := teamThreadInput{Scope: scope, Title: "Shared research", Key: hubapi.NewID("intent")}
	if scope == "project" {
		in.ProjectID = string(f.project.ID)
	}
	if scope == "issue" {
		in.IssueID = string(issue)
		in.ProjectID = "prj_ignored"
	}
	r := performHubAPIRequest(t, f.service, http.MethodPost, threadBase(f), f.token, in)
	requireNativeStatus(t, r, http.StatusCreated)
	var out teamThread
	decodeHubResponse(t, r, &out)
	return out
}

func TestTeamThreadScopeReferenceAccess(t *testing.T) {
	t.Parallel()
	t.Run("reference resource authorization", func(t *testing.T) {
		v := newVerificationFixture(t)
		v.users["owner"] = v.user(t, "thread-owner", "owner", "thread-owner@example.test", "write", "")
		base := "/api/v2/organizations/org_security/threads"
		record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "private-report.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64)}
		r := v.request(t, v.users["tester"], http.MethodPost, v.base+"/attachment-metadata", record)
		requireNativeStatus(t, r, http.StatusCreated)
		decodeHubResponse(t, r, &record)
		r = v.request(t, v.users["tester"], http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "private-report"}, Type: "bug", Title: "Private report", Body: reporterBugSections + "\n![Image](attachment:" + record.ID + ")"})
		requireNativeStatus(t, r, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, r, &issue)
		r = v.request(t, v.users["member"], http.MethodPost, v.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "private-change"}, Title: "Private change", Body: "Change details"})
		requireNativeStatus(t, r, http.StatusOK)
		var change tracker.ChangeRequest
		decodeHubResponse(t, r, &change)
		for _, reference := range []operatortool.ThreadReference{{Kind: "attachment", ID: record.ID}, {Kind: "issue", ID: string(issue.WorkItemID)}, {Kind: "change", ID: change.ID}} {
			t.Run(reference.Kind, func(t *testing.T) {
				input := teamThreadInput{Scope: "general", Title: "Shared discussion", References: []operatortool.ThreadReference{reference}}
				r := v.request(t, v.users["owner"], http.MethodPost, base, input)
				requireNativeStatus(t, r, http.StatusCreated)
				var thread teamThread
				decodeHubResponse(t, r, &thread)
				post := teamThreadInput{Kind: "note", Body: "Shared note", References: input.References}
				requireNativeStatus(t, v.request(t, v.users["owner"], http.MethodPost, base+"/"+thread.ID+"/posts", post), http.StatusCreated)
				for _, tc := range []struct {
					viewer   string
					readable bool
				}{{"reporter", false}, {"tester", true}, {"owner", true}, {"member", true}} {
					t.Run(tc.viewer, func(t *testing.T) {
						want := 0
						status := http.StatusNotFound
						if tc.readable {
							want, status = 1, http.StatusCreated
						}
						for _, target := range []string{base + "/" + thread.ID, base} {
							r := v.request(t, v.users[tc.viewer], http.MethodGet, target, nil)
							requireNativeStatus(t, r, http.StatusOK)
							var references []teamReference
							if target == base {
								var page teamThreadPage
								decodeHubResponse(t, r, &page)
								found := false
								for _, listed := range page.Threads {
									if listed.ID == thread.ID {
										references, found = listed.References, true
									}
								}
								if !found {
									t.Fatal("shared thread missing from list")
								}
							} else {
								var read teamThreadRead
								decodeHubResponse(t, r, &read)
								references = read.Thread.References
								if len(read.Posts) == 0 {
									t.Fatal("shared post missing")
								}
								for _, listed := range read.Posts {
									if len(listed.References) != want {
										t.Errorf("post references=%+v, want %d", listed.References, want)
									}
								}
							}
							if len(references) != want {
								t.Errorf("references=%+v, want %d", references, want)
							}
							if !tc.readable && strings.Contains(r.Body.String(), record.Name) {
								t.Error("unreadable attachment name disclosed")
							}
							if tc.readable && reference.Kind == "attachment" && (len(references) != 1 || references[0].Label != record.Name) {
								t.Errorf("attachment filename missing: %+v", references)
							}
						}
						requireNativeStatus(t, v.request(t, v.users[tc.viewer], http.MethodPost, base, input), status)
						requireNativeStatus(t, v.request(t, v.users[tc.viewer], http.MethodPost, base+"/"+thread.ID+"/posts", post), status)
					})
				}
			})
		}
	})
}

func TestTeamThreadHostedAgentAttribution(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "thread-member", "member", "alex@example.test", "write", "")
	credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Kind: "personal", Name: "Example client", Scope: hubapi.APIScopeOperator, Issuer: &credential, KeyScope: apikey.ScopeWrite, ProjectAccess: hubapi.HostedProjectsAll})
	if err != nil {
		t.Fatal(err)
	}
	ctx := changeOperatorContext(t, f.service, key.Token, "org_security")
	ctx = operatortool.BindConnection(ctx, "thread-agent", "Example client")
	keyCredential, _, err := f.service.authenticateTenantAPIHash(ctx, "org_security", apikey.HashToken(key.Token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx = f.service.withOperatorCatalog(ctx, keyCredential, "org_security")
	tools, err := (hostedOperatorExecutor{f.service}).ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	threadTools := 0
	for _, tool := range tools {
		if operatortool.IsThreadTool(tool.Name) {
			threadTools++
			if !strings.Contains(tool.Description, "informational") {
				t.Fatalf("missing informational description: %s", tool.Name)
			}
		}
	}
	if threadTools != 5 {
		t.Fatalf("member thread catalog contains %d tools", threadTools)
	}

	send := hostedContextProtocol(t, f.service, ctx, "stdio")
	call := func(name string, input teamThreadInput) json.RawMessage {
		t.Helper()
		data := hostedContextData(t, send("tools/call", name, input), false)
		var result struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result.Data
	}
	var thread teamThread
	if err := json.Unmarshal(call("create_thread", teamThreadInput{Scope: "project", ProjectID: string(f.project), Title: "Shared findings"}), &thread); err != nil {
		t.Fatal(err)
	}
	var post teamPost
	if err := json.Unmarshal(call("post_to_thread", teamThreadInput{ThreadID: thread.ID, Kind: "finding", Body: "Informational finding"}), &post); err != nil {
		t.Fatal(err)
	}
	if post.ActingFor != credential.ID || post.Author != credential.ID || post.Client != "fleet-test" || post.Name != "alex@example.test" {
		t.Fatalf("agent identity=%+v principal=%s", post, credential.ID)
	}
	raw := call("get_thread", teamThreadInput{ThreadID: thread.ID, Since: "last_read"})
	var read teamThreadRead
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Posts) != 1 || read.Posts[0].ActingFor != credential.ID || read.Posts[0].Client != "fleet-test" {
		t.Fatalf("read attribution=%s", raw)
	}
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE api_tokens SET operator_key_scope='read' WHERE organization_id=$1 AND id=$2", f.service.accessOrganization(), key.ID); err != nil {
		t.Fatal(err)
	}
	hostedContextData(t, send("tools/call", "post_to_thread", teamThreadInput{ThreadID: thread.ID, Kind: "note", Body: "Must be denied"}), true)
	keyCredential, _, err = f.service.authenticateTenantAPIHash(ctx, "org_security", apikey.HashToken(key.Token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx = f.service.withOperatorCatalog(ctx, keyCredential, "org_security")
	tools, err = (hostedOperatorExecutor{f.service}).ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name == "create_thread" || tool.Name == "post_to_thread" || tool.Name == "resolve_thread" {
			t.Fatalf("read key advertises %s", tool.Name)
		}
	}

}

func TestTeamThreadAuthorizationMCPRoutes(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"owner", "admin", "member", "reporter", "viewer", "removed", "ungranted", "read_only"} {
		t.Run(role, func(t *testing.T) {
			v := newVerificationFixture(t)
			if err := v.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
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
				if err := v.service.config.AccessRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(writer tenantstore.Writer) error {
					return writer.RevokeAccessMember(t.Context(), viewer.identity.Subject, v.service.config.Clock())
				}); err != nil {
					t.Fatal(err)
				}
				if err := v.provider.RevokeMembership(t.Context(), "membership_"+viewer.identity.Subject); err != nil {
					t.Fatal(err)
				}
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
					r := v.request(t, v.users[filer], http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: name}, Type: "bug", Title: "parent-" + name, Body: reporterBugSections})
					requireNativeStatus(t, r, http.StatusOK)
					var issue tracker.NativeIssue
					decodeHubResponse(t, r, &issue)
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
				if err := v.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(reader tenantstore.Reader) error {
					for index, filer := range []string{"reporter", "tester"} {
						item, err := reader.WorkItem(t.Context(), v.project, tracker.NativeWorkItemID(targets[index+2].input.IssueID))
						if err != nil {
							return err
						}
						member, err := reader.AccessMember(t.Context(), v.users[filer].identity.Subject)
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
				payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operation, "arguments": input, "_meta": json.RawMessage(fleetProtocolMeta)}})
				if err != nil {
					t.Fatal(err)
				}
				r := v.serve(t, hostedSharedRequest{user: &viewer, method: http.MethodPost, target: "/api/v2/organizations/org_security/mcp", csrf: cloudassert.CSRFToken("shared-"+viewer.identity.Subject, "org_security"), body: string(payload), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operation}})
				var envelope struct {
					Result struct {
						IsError bool `json:"isError"`
						Content struct {
							Data json.RawMessage `json:"data"`
						} `json:"structuredContent"`
					} `json:"result"`
					Error json.RawMessage `json:"error"`
				}
				if err := json.Unmarshal(r.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				ok := r.Code == http.StatusOK && !envelope.Result.IsError && len(envelope.Error) == 0 && len(envelope.Result.Content.Data) > 0
				if !ok {
					return r.Body.Bytes(), false
				}
				return envelope.Result.Content.Data, true
			}

			for _, transport := range []string{"mcp"} {
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
