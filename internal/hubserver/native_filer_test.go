package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeIssueCreatedBy(t *testing.T) {
	f := newNativeFixture(t, nil, "", "filer-list")
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	states := append(nativeTriageStates(nativeFixtureStates()), tracker.NativeState{Name: "Cancelled", Terminal: true})
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.project)
		if err != nil {
			return err
		}
		project.States = states
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE api_tokens SET name=$1 WHERE organization_id=$2 AND id=$3", "operator-filer-list", scope.organization, credential.ID); err != nil {
		t.Fatal(err)
	}

	ids := map[string]tracker.NativeWorkItemID{}
	for _, test := range []struct {
		name, state     string
		archived, other bool
	}{
		{name: "triaged", state: "Triage"},
		{name: "active", state: "Todo"},
		{name: "done", state: "Done"},
		{name: "cancelled", state: "Cancelled"},
		{name: "archived", state: "Done", archived: true},
		{name: "other", state: "Triage", other: true},
	} {
		creator := f
		if test.other {
			creator.token = testHubAdminToken
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", creator.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + test.name}, Title: test.name, Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: test.state, Labels: []string{"filed"}})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		if test.archived {
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/archive", creator.token, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive-" + test.name}, ExpectedRevision: issue.Revision})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
		}
		ids[test.name] = issue.WorkItemID
	}
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "filer-foreign")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: scope.organization, TokenID: credential.ID, ProjectID: foreign.project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	foreign.token = f.token
	hidden := foreign.create(t, "hidden filer")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessTokenGrant(t.Context(), credential.ID, foreign.project.ID)
	}); err != nil {
		t.Fatal(err)
	}

	drainTestNativeBoard(t, f.service)
	call := hostedContextProtocol(t, f.service, changeOperatorContext(t, f.service, f.token, string(scope.organization)), "http")
	for _, test := range []struct {
		name, creator, state, archived, query, label string
		want                                         []string
	}{
		{name: "unchanged", want: []string{"triaged", "active", "done", "cancelled", "other"}},
		{name: "principal", creator: credential.ID, want: []string{"triaged", "active", "done", "cancelled"}},
		{name: "me", creator: "me", want: []string{"triaged", "active", "done", "cancelled"}},
		{name: "triage", creator: "me", state: "Triage", want: []string{"triaged"}},
		{name: "all archives", creator: "me", archived: "all", want: []string{"triaged", "active", "done", "cancelled", "archived"}},
		{name: "archived done", creator: "me", archived: "true", state: "Done", want: []string{"archived"}},
		{name: "cancelled", creator: "me", state: "Cancelled", want: []string{"cancelled"}},
		{name: "combined", creator: "me", state: "Triage", archived: "all", query: "triaged", label: "filed", want: []string{"triaged"}},
		{name: "excluded label", creator: "me", label: "missing", want: []string{}},
		{name: "missing principal", creator: "missing", want: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := []tracker.NativeWorkItemID{}
			for _, name := range test.want {
				want = append(want, ids[name])
			}
			slices.Sort(want)
			for _, projection := range []string{"summary", "board", "mcp"} {
				params := url.Values{"created_by": {test.creator}, "state": {test.state}, "q": {test.query}, "label": {test.label}, "limit": {"1"}}
				if test.archived != "" {
					params.Set("archived", test.archived)
				}
				params.Set("include", projection)
				got := []tracker.NativeWorkItemID{}
				for {
					var page tracker.NativeIssuePage
					if projection == "mcp" {
						args := map[string]any{"project_id": scope.project, "limit": 1}
						for key, value := range map[string]string{"created_by": test.creator, "state": test.state, "archived": test.archived, "query": test.query, "label": test.label, "cursor": params.Get("cursor")} {
							if value != "" {
								args[key] = value
							}
						}
						data := hostedContextData(t, call("tools/call", operatortool.WorkList, args), false)
						var result operatortool.WorkReadResult[tracker.NativeIssuePage]
						if err := json.Unmarshal(data, &result); err != nil {
							t.Fatal(err)
						}
						page = result.Data
					} else {
						response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+params.Encode(), f.token, nil)
						if response.Code != http.StatusOK {
							_, err := f.service.readIssues(t.Context(), scope, params)
							t.Fatalf("%s read error: %v response: %s", projection, err, response.Body.String())
						}
						decodeHubResponse(t, response, &page)
					}
					for _, issue := range page.Items {
						if issue.WorkItemID == hidden.WorkItemID {
							t.Fatal("foreign item exposed")
						}
						if issue.Actor.PrincipalID == credential.ID && issue.Actor.DisplayName != "operator-filer-list" {
							t.Fatalf("filer = %+v", issue.Actor)
						}
						got = append(got, issue.WorkItemID)
					}
					if page.NextCursor == "" {
						break
					}
					if len(got) > len(ids) {
						t.Fatal("cursor did not advance")
					}
					params.Set("cursor", page.NextCursor)
				}
				slices.Sort(got)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s items = %v, want %v", projection, got, want)
				}
			}
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(ids["triaged"]), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var restDetail tracker.NativeIssue
	decodeHubResponse(t, response, &restDetail)
	if restDetail.Actor.Kind != "human" || restDetail.Actor.PrincipalID != credential.ID || restDetail.Actor.DisplayName != "operator-filer-list" {
		t.Fatalf("REST detail filer=%+v", restDetail.Actor)
	}
	data := hostedContextData(t, call("tools/call", operatortool.WorkItem, map[string]any{"project_id": scope.project, "reference": ids["triaged"]}), false)
	var detail operatortool.WorkReadResult[tracker.NativeIssue]
	if err := json.Unmarshal(data, &detail); err != nil || detail.Data.Actor.DisplayName != "operator-filer-list" {
		t.Fatalf("detail=%s error=%v", data, err)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, foreign.base+"/work-items?created_by=me", f.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
	hostedContextData(t, call("tools/call", operatortool.WorkList, map[string]any{"project_id": foreign.project.ID, "created_by": "me"}), true)
}

func TestNativeFilerPublicNames(t *testing.T) {
	f := newNativeFixture(t, nil, "", "filer-names")
	organizations := []tracker.OrganizationID{f.project.OrganizationID, f.project.OrganizationID + "_second"}
	for _, organization := range organizations {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, principal := range []string{"person", "service"} {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: string(organization) + " <" + principal + ">", Hash: apikey.HashToken(string(organization) + principal), Scope: "operator", Kind: "service", ProjectAccess: "all", CreatedAt: f.service.config.now(), UpdatedAt: f.service.config.now()}); err != nil {
					return err
				}
			}
			return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_person", PrincipalID: "person", MembershipID: "membership_person", Email: string(organization) + "@example.test", Role: "member", Active: true, CreatedAt: f.service.config.now(), UpdatedAt: f.service.config.now()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range organizations {
		t.Run(string(organization), func(t *testing.T) {
			var names map[string]string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				names, err = r.AccessPublicNames(t.Context(), []string{"person", "service", "missing", "person"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(names) != 3 || names["person"] != string(organization)+"@example.test" || names["service"] != string(organization)+" <service>" || names["missing"] != "" {
				t.Fatalf("public identities=%v", names)
			}
			var records []viewstore.BoardRecord
			for _, principal := range []string{"person", "service"} {
				issue := tracker.NativeIssue{Actor: tracker.Actor{Kind: "human", PrincipalID: principal}}
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, viewstore.BoardRecord{PageIssue: &issue, ListBytes: len(raw)})
			}
			work := append([]viewstore.BoardRecord(nil), records...)
			queries := 0
			ctx := postgres.WithTransactionTiming(t.Context(), func(string, time.Time) {}, func(_ time.Time, query string) {
				if strings.Contains(query, "FROM unnest($2::text[])") {
					queries++
				}
			})
			if err := f.service.hydrateBoardRecordActorNames(ctx, organization, records, work); err != nil {
				t.Fatal(err)
			}
			if queries != 1 {
				t.Fatalf("page filer lookups=%d, want one batch", queries)
			}
			for _, group := range [][]viewstore.BoardRecord{records, work} {
				for _, row := range group {
					actor := row.PageIssue.Actor
					if actor.Kind != "human" || actor.DisplayName != names[actor.PrincipalID] {
						t.Fatalf("hydrated actor=%+v", actor)
					}
					raw, err := json.Marshal(row.PageIssue)
					if err != nil || row.ListBytes != len(raw) {
						t.Fatalf("page bytes=%d encoded=%d error=%v", row.ListBytes, len(raw), err)
					}
				}
			}
			currentName, missingName := names["service"], ""
			for _, test := range []struct {
				name, principal, stored, want string
				loaded                        *string
				queries                       int
			}{
				{"loaded current", "service", "Previous name", currentName, &currentName, 0},
				{"loaded absent", "missing", "External <actor>", "External <actor>", &missingName, 0},
				{"unloaded absent", "missing", "External <actor>", "External <actor>", nil, 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					original := tracker.NativeIssue{Actor: tracker.Actor{Kind: "integration", PrincipalID: test.principal, ViaKey: "recorded-key", DisplayName: test.stored, Agent: "Recorded assistant"}}
					raw, err := json.Marshal(original)
					if err != nil {
						t.Fatal(err)
					}
					records := []viewstore.BoardRecord{{PageIssue: &original, ActorDisplayName: test.loaded, ListBytes: len(raw)}}
					work := append([]viewstore.BoardRecord(nil), records...)
					queries = 0
					if err := f.service.hydrateBoardRecordActorNames(ctx, organization, records, work); err != nil {
						t.Fatal(err)
					}
					if queries != test.queries || original.Actor.DisplayName != test.stored || records[0].PageIssue == work[0].PageIssue {
						t.Fatalf("lookups=%d want=%d original=%+v", queries, test.queries, original.Actor)
					}
					want := original.Actor
					want.DisplayName = test.want
					for _, row := range []viewstore.BoardRecord{records[0], work[0]} {
						if row.PageIssue == &original || row.PageIssue.Actor != want {
							t.Fatalf("hydrated actor=%+v want=%+v", row.PageIssue.Actor, want)
						}
						raw, err := json.Marshal(row.PageIssue)
						if err != nil || row.ListBytes != len(raw) {
							t.Fatalf("page bytes=%d encoded=%d error=%v", row.ListBytes, len(raw), err)
						}
					}
				})
			}
		})
	}
}

func TestNativeCreatedByIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential apiCredential
		want       string
	}{
		{"browser", apiCredential{ID: "person", SessionHash: "session"}, "person"},
		{"personal hosted", apiCredential{ID: "key", HostedPrincipal: "person", HostedKeyKind: "personal", HostedKeyScope: apikey.ScopeRead}, "person"},
		{"personal entry", apiCredential{ID: "person", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "personal"}}, "person"},
		{"service hosted", apiCredential{ID: "key", HostedPrincipal: "person", HostedKeyKind: "service", HostedKeyScope: apikey.ScopeRead}, ""},
		{"service entry", apiCredential{ID: "service", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "service"}}, ""},
		{"worker", apiCredential{ID: "worker", Scope: apiScopeWorker}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := nativeCreatedBy(nativeScope{credential: test.credential}, url.Values{"created_by": {"me"}})
			if test.want == "" {
				var native *nativeError
				if !errors.As(err, &native) || native.Code != "invalid_request" {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil || value != test.want {
				t.Fatalf("principal=%s error=%v", value, err)
			}
		})
	}
}

func TestHostedPersonalKeyCreatedBy(t *testing.T) {
	f := newHostedSecurityFixture(t)
	users := []hostedSecurityUser{
		f.user(t, "filer", "reporter", "filer@example.test", "write", ""),
		f.user(t, "other_filer", "member", "other@example.test", "write", ""),
	}
	var mine tracker.NativeIssue
	var personal tracker.NativeIssue
	for index, user := range users {
		issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org_security", project: f.project, credential: issuer}
		if index == 0 {
			if issuer.HostedRole != "reporter" {
				t.Fatalf("filer role=%q", issuer.HostedRole)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := updateNativeProjectStates(t.Context(), tx, scope, nativeFixtureStates(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		response := createIndependentNativeIssue(t, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-filer"}, Title: "Filed issue", Type: "bug", Body: reporterBugSections + issueContractTestSections, State: "Todo"}, func(request tracker.CreateIssue) *httptest.ResponseRecorder {
			return f.request(t, user, http.MethodPost, f.base+"/work-items", request)
		})
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		if index == 0 {
			mine = issue
		}
	}
	user := users[0]
	issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"personal", "service"} {
		key, err := f.service.createHostedAPIKeyFor(t.Context(), issuer, hostedKeyRequest{Kind: kind, Name: "filer-" + kind, Scope: apikey.ScopeWrite, NeverExpires: true, ProjectAccess: hostedProjectsAll})
		if err != nil {
			t.Fatal(err)
		}
		if kind == "personal" {
			response := createIndependentNativeIssue(t, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "personal-filer"}, Title: "Personal key issue", Type: "bug", Body: reporterBugSections + issueContractTestSections, State: "Todo"}, func(request tracker.CreateIssue) *httptest.ResponseRecorder {
				return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", key.Token, request)
			})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &personal)
			if personal.State != "Triage" || personal.Actor.Kind != "human" || personal.Actor.PrincipalID != mine.Actor.PrincipalID || personal.Actor.ViaKey != key.ID {
				t.Fatalf("personal issue=%+v", personal)
			}
			f.grant(t, user, false, false)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?created_by=me", key.Token, nil)
		var restPage tracker.NativeIssuePage
		if kind == "service" {
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			var refusal apiErrorResponse
			decodeHubResponse(t, response, &refusal)
			if refusal.Code != "invalid_request" {
				t.Fatalf("refusal=%+v", refusal)
			}
		} else {
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &restPage)
		}

		payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.WorkList, "arguments": map[string]any{"project_id": f.project, "created_by": "me"}, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "filer-test", "version": "1"}}}}
		response = performHubWorkCall(t, f.service, "/api/v2/organizations/org_security/mcp", key.Token, operatortool.WorkList, payload)
		requireNativeStatus(t, response, http.StatusOK)
		var reply hostedContextReply
		decodeHubResponse(t, response, &reply)
		if kind == "service" {
			if !slices.ContainsFunc([]string{string(reply.Result), string(reply.Error)}, func(value string) bool { return strings.Contains(value, "invalid_request") }) {
				t.Fatalf("service error=%s %s", reply.Result, reply.Error)
			}
			continue
		}
		data := hostedContextData(t, reply, false)
		var result operatortool.WorkReadResult[tracker.NativeIssuePage]
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		want := []tracker.NativeWorkItemID{mine.WorkItemID, personal.WorkItemID}
		slices.Sort(want)
		for projection, page := range map[string]tracker.NativeIssuePage{"rest": restPage, "mcp": result.Data} {
			got := []tracker.NativeWorkItemID{}
			for _, issue := range page.Items {
				if issue.Actor.DisplayName != user.identity.Email {
					t.Fatalf("%s display name=%q", projection, issue.Actor.DisplayName)
				}
				got = append(got, issue.WorkItemID)
			}
			slices.Sort(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s personal key items=%+v", projection, page.Items)
			}
		}
	}
}
