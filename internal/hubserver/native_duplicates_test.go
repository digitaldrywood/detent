package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDuplicateCandidateCrowding(t *testing.T) {
	for _, crowd := range []string{"project", "organization", "archived", "invisible", "partial coverage", "eligible"} {
		t.Run(crowd, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			target := f.createIndependent(t, "Separate issue context")
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(target.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "crowding-comment"}, Body: "Repository association deadlocks"})
			requireNativeStatus(t, response, http.StatusOK)
			partial := f.createIndependent(t, "Repository association")
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			credential.HostedRole = "reporter"
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential, sourceActor: &target.Actor}
			creator := f
			if crowd == "project" || crowd == "organization" || crowd == "invisible" {
				organization := f.project.OrganizationID
				if crowd == "organization" {
					organization += "_second"
				}
				creator = newNativeFixture(t, f.service, organization, "foreign-crowding")
				if crowd == "invisible" {
					foreign, _, err := f.service.authenticateTenantAPIHash(t.Context(), organization, apikey.HashToken(creator.token), "", "")
					if err != nil {
						t.Fatal(err)
					}
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: foreign.ID, ProjectID: f.project.ID})
					}); err != nil {
						t.Fatal(err)
					}
					creator.project, creator.base = f.project, f.base
				}
			}
			for number := 3; number <= 127; number++ {
				title := "Repository association deadlocks " + strconv.Itoa(number)
				if crowd == "partial coverage" {
					title = "association " + strconv.Itoa(number)
				}
				item := creator.createIndependent(t, title)
				if crowd == "archived" {
					response := performHubAPIRequest(t, creator.service, http.MethodPost, creator.base+"/work-items/"+string(item.WorkItemID)+"/archive", creator.token, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive-" + string(item.WorkItemID)}, ExpectedRevision: item.Revision})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			for _, org := range []tracker.OrganizationID{f.project.OrganizationID, creator.project.OrganizationID} {
				refreshNativeDuplicateSearchFixture(t, f.service, org)
			}
			for _, text := range []string{"repos associ deadlock", "repository association absent"} {
				ids := map[tracker.NativeWorkItemID]bool{}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					hits, err := r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: scope.project, Reporter: new(target.Actor.PrincipalID), Text: text, Limit: 200})
					for _, hit := range hits {
						ids[hit.Document.WorkItemID] = true
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				want := 2
				if crowd == "eligible" || crowd == "partial coverage" {
					want = 127
				}
				if len(ids) != want || !ids[target.WorkItemID] || !ids[partial.WorkItemID] {
					t.Fatalf("search=%q ids=%v want %d matches", text, ids, want)
				}
			}
			if crowd == "eligible" {
				return
			}
			for _, request := range []tracker.CreateIssue{{Title: "Repository association deadlocks"}, {Title: "Separate observed symptom", Body: "Repository association deadlocks"}} {
				matches, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, request, false)
				if err != nil || matches == nil || !matches.available || len(matches.matches) != 2 || !slices.ContainsFunc(matches.matches, func(hit possibleDuplicate) bool { return hit.WorkItemID == target.WorkItemID }) || !slices.ContainsFunc(matches.matches, func(hit possibleDuplicate) bool { return hit.WorkItemID == partial.WorkItemID }) {
					t.Fatalf("request=%+v matches=%+v err=%v", request, matches, err)
				}
			}
		})
	}
}

func TestNativeSearchScopedCost(t *testing.T) {
	for _, state := range []string{"ready", "rebuilding", "dirty"} {
		t.Run(state, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			target := f.createIndependent(t, "Repository association")
			foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-search-cost")
			seed := foreign.createIndependent(t, "Repository association deadlocks")
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			adminValue, ok := projectRecordDatabases.Load(f.service.database.postgres)
			if !ok {
				t.Fatal("fixture PostgreSQL administration connection unavailable")
			}
			admin := adminValue.(*sql.DB)
			count := 1
			scores := map[string]float64{}
			for _, size := range []int{1, 300, 3000, 6000} {
				t.Run(strconv.Itoa(size), func(t *testing.T) {
					if size > count {
						if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,project_id,native_id,number,revision,title,body_object_key,issue_contract_object_key,actor_kind,actor_principal_id,workflow_state_name,created_at,updated_at,native_created_at,native_updated_at,last_activity_at)
SELECT organization_id,project_id,'wi_scoped_cost_'||n,n,1,title,body_object_key,issue_contract_object_key,actor_kind,actor_principal_id,workflow_state_name,created_at,updated_at,native_created_at,native_updated_at,last_activity_at FROM issues CROSS JOIN generate_series($1::bigint,$2::bigint) n WHERE organization_id=$3 AND project_id=$4 AND native_id=$5`, count+1, size, f.project.OrganizationID, foreign.project.ID, seed.WorkItemID); err != nil {
							t.Fatal(err)
						}
						count = size
					}
					refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
					var indexed int
					if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM search_documents WHERE organization_id=$1 AND project_id=$2 AND kind='issue' AND NOT placeholder AND indexed_revision=revision", f.project.OrganizationID, foreign.project.ID).Scan(&indexed); err != nil || indexed != size {
						t.Fatalf("foreign index population=%d want=%d error=%v", indexed, size, err)
					}
					if state != "ready" {
						response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(target.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "pending-cost-" + strconv.Itoa(size)}, ExpectedRevision: target.Revision, Labels: &[]string{"scope-marker-" + strconv.Itoa(size)}})
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &target)
						if state == "dirty" {
							response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(target.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "dirty-cost-comment-" + strconv.Itoa(size)}, Body: "Separate context"})
							requireNativeStatus(t, response, http.StatusOK)
						}
					}
					for _, text := range []string{"repos associ deadlock", "repository repository association association"} {
						if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							hits, err := r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: f.project.ID, Text: text, Limit: 100})
							if err != nil {
								return err
							}
							ids := map[tracker.NativeWorkItemID]bool{}
							var score float64
							for _, hit := range hits {
								ids[hit.Document.WorkItemID] = true
								score += hit.Score
								if hit.Coverage != 2 {
									t.Fatalf("hits=%+v want 2 covered terms", hits)
								}
							}
							if len(ids) != 1 || !ids[target.WorkItemID] {
								t.Fatalf("foreign population %d displaced scoped result: %+v", size, hits)
							}
							if size == 1 {
								scores[text] = score
							}
							if state != "ready" && score != scores[text] {
								t.Fatalf("fallback score changed with foreign inventory: got=%v want=%v", score, scores[text])
							}
							reader := r.(searchstore.DuplicateSourceReader)
							availability, err := reader.DuplicateSourceState(t.Context(), f.project.ID)
							if err != nil {
								return err
							}
							if availability.Ready != (state == "ready") {
								t.Fatalf("index ready=%t for state %s", availability.Ready, state)
							}
							page, err := reader.DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: f.project.ID, Text: "Repository association deadlocks", Now: f.service.config.now()})
							if state != "ready" {
								if !errors.Is(err, searchstore.ErrIndexPending) {
									t.Fatalf("incomplete duplicate source=%+v err=%v", page, err)
								}
								return nil
							}
							if err != nil {
								return err
							}
							if len(page.Sources) != 1 || page.Sources[0].Document.WorkItemID != target.WorkItemID {
								t.Fatalf("foreign population %d crowded duplicate source: %+v", size, page)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					duplicates, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, tracker.CreateIssue{Title: "Repository association deadlocks"}, false)
					want := 1
					if state != "ready" {
						want = 0
					}
					if err != nil || duplicates == nil || len(duplicates.matches) != want || duplicates.available != (state == "ready") || want == 1 && duplicates.matches[0].WorkItemID != target.WorkItemID {
						t.Fatalf("Hub duplicate outcome for state %s with %d indexed foreign issues: %+v error=%v", state, size, duplicates, err)
					}
				})
			}
		})
	}
}

func refreshNativeDuplicateSearchFixture(t *testing.T, service *Service, organization tracker.OrganizationID) {
	t.Helper()
	for range 20000 {
		more, err := searchstore.NewContent(service.runDetailStore()).RefreshOne(t.Context(), tenantSearchRepository{service.config.TenantRepository}, organization)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("search fixture did not finish its bounded source population")
}

func TestNativeDuplicateFiling(t *testing.T) {
	for _, test := range []struct {
		name       string
		enabled    bool
		title      string
		body       string
		closedDays int
		challenge  bool
		archived   bool
		state      string
		failRead   bool
	}{
		{"matching title", true, "Runner checkout deadlocks", "Separate observed details", 0, true, false, "Todo", false},
		{"default lane", true, "Runner checkout deadlocks", "Separate observed details", 0, true, false, "", false},
		{"matching body", true, "A different symptom", "Checkout hangs during repository association", 0, true, false, "Todo", false},
		{"no match", true, "Invitation email rejected", "Mailbox rejects the invite address; delivery returns an SMTP rejection from the recipient domain after verification links are sent. Sender reputation, relay credentials, mailbox quota, attachments, calendar invitations, spam filtering, and address formatting each need investigation.", 0, false, false, "Todo", false},
		{"disabled", false, "Runner checkout deadlocks", "Checkout hangs during repository association", 0, false, false, "Todo", false},
		{"recent closed", true, "Runner checkout deadlocks", "Separate observed details", 29, true, false, "Todo", false},
		{"old closed", true, "Runner checkout deadlocks", "Separate observed details", 31, false, false, "Todo", false},
		{"recent archived", true, "Runner checkout deadlocks", "Separate observed details", 29, false, true, "Todo", false},
		{"recent archived body", true, "A different symptom", "Checkout hangs during repository association", 29, false, true, "Todo", false},
		{"unlinked Triage", true, "Runner checkout deadlocks", "Separate observed details", 0, false, false, "Triage", false},
		{"post-commit failure", true, "Runner checkout deadlocks", "Separate observed details", 0, true, false, "Todo", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC).AddDate(0, 0, -test.closedDays)
			service := newClockedPostgresNativeService(t, Config{now: func() time.Time { return now }})
			f := newNativeFixture(t, service, "", "duplicates")
			first := f.create(t, "Runner checkout deadlocks")
			body := "Checkout hangs during repository association" + issueContractTestSections
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(first.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "candidate-body"}, ExpectedRevision: first.Revision, Body: &body})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &first)
			foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign")
			foreign.create(t, test.title)
			setNativeDuplicateChallengeFixture(t, f, test.enabled)
			if test.closedDays != 0 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close"}, ExpectedRevision: first.Revision, State: "Done", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &first)
				now = now.AddDate(0, 0, test.closedDays)
			}
			if test.archived {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/archive", f.token, archiveRequest(first, "archive-candidate"))
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &first)
			}
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "filing"}, Title: test.title, Body: test.body + issueContractTestSections, State: test.state}
			searchTrace := installNativeBrowserPathFault(t, f.service)
			searchTrace.observe.Store(true)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			searchTrace.observe.Store(false)
			if !test.enabled && (searchTrace.readerSearches.Load() != 0 || searchTrace.writerSearches.Load() != 0) {
				t.Fatalf("disabled challenge searched: reader=%d writer=%d", searchTrace.readerSearches.Load(), searchTrace.writerSearches.Load())
			}
			if test.enabled && test.challenge && searchTrace.readerSearches.Load()+searchTrace.writerSearches.Load() == 0 {
				t.Fatal("enabled challenge did not exercise the PostgreSQL search tracer")
			}
			wantStatus := http.StatusOK
			if test.challenge {
				wantStatus = http.StatusConflict
			}
			if test.state == "Triage" {
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				var refusal struct {
					hubapi.APIErrorResponse
					duplicateChallenge
				}
				decodeHubResponse(t, response, &refusal)
				if refusal.Code != "invalid_request" || refusal.Message != "Triage is reserved for linked GitHub issue intake" || refusal.ChallengeToken != "" {
					t.Fatalf("lane refusal=%s", response.Body.String())
				}
				var issues, challenges int
				if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&issues); err != nil {
					t.Fatal(err)
				}
				if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&challenges); err != nil {
					t.Fatal(err)
				}
				if issues != 1 || challenges != 0 {
					t.Fatalf("refused lane created issues=%d challenges=%d", issues, challenges)
				}
				return
			}
			requireNativeStatus(t, response, wantStatus)
			var challenge duplicateChallenge
			decodeHubResponse(t, response, &challenge)
			if test.challenge {
				var failure hubapi.APIErrorResponse
				decodeHubResponse(t, response, &failure)
				if failure.Code != "possible_duplicates" || failure.Message == "" {
					t.Fatalf("challenge omitted API error contract: %s", response.Body.String())
				}
			}
			if (challenge.ChallengeToken != "") != test.challenge {
				t.Fatalf("challenge = %s", response.Body.String())
			}
			var count int
			if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if !test.challenge {
				var created tracker.NativeIssue
				decodeHubResponse(t, response, &created)
				if count != 2 || created.Title != test.title || created.WorkItemID == first.WorkItemID {
					t.Fatalf("issue was not filed immediately: %s", response.Body.String())
				}
				return
			}
			if count != 1 || len(challenge.PossibleDuplicates) != 1 || challenge.PossibleDuplicates[0].WorkItemID != first.WorkItemID || challenge.PossibleDuplicates[0].Identifier != string(f.project.ID)+"#"+strconv.Itoa(first.Number) || !strings.HasPrefix(challenge.PossibleDuplicates[0].Why, "Matching terms: ") {
				t.Fatalf("unexpected matches or issue created: %s, count %d", response.Body.String(), count)
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, replay, http.StatusConflict)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("challenge replay changed")
			}
			request.ChallengeToken = challenge.ChallengeToken
			request.NotDuplicateOf = []int{first.Number}
			var fault *nativeBrowserPathFault
			if test.failRead {
				fault = installNativeBrowserPathFault(t, f.service)
				fault.enabled.Store(true)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var created tracker.NativeIssue
			decodeHubResponse(t, response, &created)
			if created.WorkItemID == "" || created.WorkItemID == first.WorkItemID || len(created.Dependencies) != 0 || len(created.Blockers) != 0 {
				t.Fatalf("confirmation did not create nonblocking work: %#v", created)
			}
			replay = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("confirmation replay changed")
			}
			if fault != nil {
				if fault.failures.Load() != 0 || slices.Contains(created.OmittedFields, "web_url") {
					t.Fatalf("receipt performed a late browser path read: failures=%d response=%s", fault.failures.Load(), response.Body.String())
				}
				fault.enabled.Store(false)
				if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != 2 {
					t.Fatalf("confirmation or replay duplicated item: count=%d err=%v", count, err)
				}
			}
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			reader := operatorWorkReads{service: f.service, scope: nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}}
			for _, pair := range [][2]tracker.NativeWorkItemID{{first.WorkItemID, created.WorkItemID}, {created.WorkItemID, first.WorkItemID}} {
				result, err := reader.ReadWork(ctx, operatortool.WorkRelationships, operatortool.WorkReadRequest{ProjectID: string(f.project.ID), Reference: string(pair[0])})
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Data struct {
						Related      []tracker.NativeDependency `json:"related"`
						Dependencies []tracker.NativeWorkItemID `json:"dependencies"`
					} `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &envelope); err != nil {
					t.Fatal(err)
				}
				if len(envelope.Data.Related) != 1 || envelope.Data.Related[0].ID != pair[1] || len(envelope.Data.Dependencies) != 0 {
					t.Fatalf("relationships not symmetric: %s", result.Content)
				}
			}
		})
	}
}

func TestNativeDuplicateConfirmationRefusals(t *testing.T) {
	f := newNativeFixture(t, nil, "", "token-binding")
	first := f.create(t, "Runner checkout deadlocks")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+credential.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": other.project.ID}), http.StatusNoContent)
	setNativeDuplicateChallengeFixture(t, f, true)
	refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "bound"}, Title: first.Title, Body: "Independent description" + issueContractTestSections, State: "Todo"}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	requireNativeStatus(t, response, http.StatusConflict)
	var challenge duplicateChallenge
	decodeHubResponse(t, response, &challenge)
	request.ChallengeToken = challenge.ChallengeToken
	request.NotDuplicateOf = []int{first.Number}
	for _, test := range []string{"title", "body", "request id", "principal", "project", "forged", "unlisted", "duplicate numbers"} {
		t.Run(test, func(t *testing.T) {
			r := request
			base, token := f.base, f.token
			switch test {
			case "title":
				r.Title += " changed"
			case "body":
				r.Body += " changed"
			case "request id":
				r.IdempotencyKey = "another-request"
			case "principal":
				token = testHubAdminToken
			case "project":
				base = other.base
			case "forged":
				r.ChallengeToken = "dup_unknown"
			case "unlisted":
				r.NotDuplicateOf = []int{999}
			case "duplicate numbers":
				r.NotDuplicateOf = []int{first.Number, first.Number}
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/work-items", token, r)
			if test == "unlisted" || test == "duplicate numbers" {
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				return
			}
			requireNativeCode(t, response, http.StatusGone, "idempotency_expired")
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	request.IdempotencyKey = "reuse"
	requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusGone, "idempotency_expired")
	refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
	request.IdempotencyKey = "second-challenge"
	request.ChallengeToken, request.NotDuplicateOf = "", nil
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	requireNativeStatus(t, response, http.StatusConflict)
	var second duplicateChallenge
	decodeHubResponse(t, response, &second)
	if second.ChallengeToken == "" || second.ChallengeToken == challenge.ChallengeToken {
		t.Fatal("independent filing did not get its own usable challenge")
	}
	request.ChallengeToken, request.NotDuplicateOf = second.ChallengeToken, []int{first.Number}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusOK)
	var count int
	if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("refused confirmations created issues: %d", count)
	}
}

func TestNativeDuplicateChallengeLifetime(t *testing.T) {
	for _, test := range []struct {
		name string
		age  time.Duration
		want int
	}{
		{"fresh", 0, http.StatusOK},
		{"before expiry", time.Hour - time.Second, http.StatusOK},
		{"expiry boundary", time.Hour, http.StatusGone},
		{"expired", time.Hour + time.Second, http.StatusGone},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
			service := newClockedPostgresNativeService(t, Config{now: func() time.Time { return now }})
			f := newNativeFixture(t, service, "", "lifetime")
			first := f.create(t, "Runner checkout deadlocks")
			setNativeDuplicateChallengeFixture(t, f, true)
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "lifetime"}, Title: first.Title, Body: "Independent description" + issueContractTestSections, State: "Todo"}
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			var challenge duplicateChallenge
			decodeHubResponse(t, response, &challenge)
			if challenge.ChallengeToken == "" {
				t.Fatalf("no challenge: %s", response.Body.String())
			}
			request.ChallengeToken, request.NotDuplicateOf = challenge.ChallengeToken, []int{first.Number}
			now = now.Add(test.age)
			response = performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, test.want)
			var count int
			if err := conversationTestAdmin(t, service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			wantCount := 1
			if test.want == http.StatusOK {
				wantCount = 2
			}
			if count != wantCount {
				t.Fatalf("issue count=%d, want %d", count, wantCount)
			}
			if test.want == http.StatusOK {
				if err := conversationTestAdmin(t, service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1 AND token=$2", f.project.OrganizationID, challenge.ChallengeToken).Scan(&count); err != nil || count != 0 {
					t.Fatalf("consumed challenge retained: %d, %v", count, err)
				}
			}
			if err := service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			if err := conversationTestAdmin(t, service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1 AND token=$2", f.project.OrganizationID, challenge.ChallengeToken).Scan(&count); err != nil || count != 0 {
				t.Fatalf("challenge survived cleanup: %d, %v", count, err)
			}
			if test.want == http.StatusOK {
				now = now.Add(2 * time.Hour)
				replay := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
				requireNativeStatus(t, replay, http.StatusOK)
				if replay.Body.String() != response.Body.String() {
					t.Fatal("confirmation replay changed after challenge deletion and expiry")
				}
			}
		})
	}
}

func TestNativeDuplicateChallengeSettings(t *testing.T) {
	f := newNativeFixture(t, nil, "", "settings")
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "default-setting", "name": "Default duplicate setting", "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	f.project = project
	f.base = "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID)
	var current ProjectIntegration
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/integration", testHubAdminToken, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &current)
	if !current.DuplicateChallenge {
		t.Fatal("new project must challenge by default")
	}
	request := updateProjectIntegrationRequest{Mutation: tracker.Mutation{IdempotencyKey: "setting"}, ExpectedRevision: current.Revision, Intake: "disabled", Projection: "disabled", DuplicateChallenge: new(false)}
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+credential.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": project.OrganizationID, "project_id": project.ID}), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", f.token, request), http.StatusForbidden)
	response = performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusOK)
	var updated ProjectIntegration
	decodeHubResponse(t, response, &updated)
	if updated.DuplicateChallenge || updated.Revision != current.Revision+1 {
		t.Fatalf("setting not revisioned: %#v", updated)
	}
	replay := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != response.Body.String() {
		t.Fatal("settings replay changed")
	}
	request.IdempotencyKey = "stale"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/integration", testHubAdminToken, request), http.StatusConflict)
}

type nativeBrowserPathFault struct {
	observe        atomic.Bool
	readerSearches atomic.Int64
	writerSearches atomic.Int64
	enabled        atomic.Bool
	failures       atomic.Int64
}

type nativeDuplicateRecheckKey struct{}

type nativeBrowserPathRepository struct {
	*postgres.Repository
	delegate    tenantstore.Repository
	fault       *nativeBrowserPathFault
	beforeWrite func()
	writes      atomic.Int64
}

type nativeBrowserPathAuthorityRepository struct {
	*nativeBrowserPathRepository
	authority tenantauth.Reader
}

func (r *nativeBrowserPathAuthorityRepository) ReadTenantAuthority(ctx context.Context, input tenantauth.RequestIdentity) (tenantauth.Scope, error) {
	return r.authority.ReadTenantAuthority(ctx, input)
}

func (r *nativeBrowserPathRepository) ReadRequest(ctx context.Context, organization tracker.OrganizationID, run func(context.Context) error) error {
	return r.delegate.(tenantstore.RequestReader).ReadRequest(ctx, organization, run)
}

func (r *nativeBrowserPathRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.delegate.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(&nativeBrowserPathReader{Reader: reader, WorkItemRelationshipBatchReader: reader.(tenantstore.WorkItemRelationshipBatchReader), DuplicateChallengeReader: reader.(corestore.DuplicateChallengeReader), DuplicateSourceReader: reader.(searchstore.DuplicateSourceReader), fault: r.fault})
	})
}

type nativeBrowserPathReader struct {
	tenantstore.Reader
	tenantstore.WorkItemRelationshipBatchReader
	corestore.DuplicateChallengeReader
	searchstore.DuplicateSourceReader
	fault       *nativeBrowserPathFault
	sawWorkItem bool
}

func (r *nativeBrowserPathReader) WorkItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	value, err := r.Reader.WorkItem(ctx, project, item)
	r.sawWorkItem = err == nil
	return value, err
}

func (r *nativeBrowserPathReader) WorkItemByNativeID(ctx context.Context, item tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	value, err := r.Reader.WorkItemByNativeID(ctx, item)
	r.sawWorkItem = err == nil
	return value, err
}

func (r *nativeBrowserPathReader) Project(ctx context.Context, project tracker.ProjectID) (corestore.Project, error) {
	if r.sawWorkItem && r.fault.enabled.Load() {
		r.fault.failures.Add(1)
		return corestore.Project{}, errors.New("injected browser path read failure")
	}
	return r.Reader.Project(ctx, project)
}

func (f *nativeBrowserPathFault) queryContext(ctx context.Context, writer bool) context.Context {
	return postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, statement string) {
		if f.observe.Load() && strings.Contains(statement, "search_documents") {
			if writer {
				f.writerSearches.Add(1)
			} else {
				f.readerSearches.Add(1)
			}
		}
	})
}

func (r *nativeBrowserPathReader) DuplicateSources(ctx context.Context, query searchstore.DuplicateSourceQuery) (searchstore.DuplicateSourcePage, error) {
	return r.DuplicateSourceReader.DuplicateSources(r.fault.queryContext(ctx, false), query)
}

type nativeBrowserPathWriter struct {
	tenantstore.Writer
	tenantstore.WorkItemRelationshipBatchReader
	corestore.DuplicateChallengeWriter
	searchstore.DuplicateSourceWriter
	searchstore.DuplicateSourceReader
	fault *nativeBrowserPathFault
}

func (w *nativeBrowserPathWriter) DuplicateSources(ctx context.Context, query searchstore.DuplicateSourceQuery) (searchstore.DuplicateSourcePage, error) {
	return w.DuplicateSourceReader.DuplicateSources(w.fault.queryContext(ctx, true), query)
}

func (r *nativeBrowserPathRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.delegate.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(&nativeBrowserPathReader{Reader: reader, WorkItemRelationshipBatchReader: reader.(tenantstore.WorkItemRelationshipBatchReader), DuplicateChallengeReader: reader.(corestore.DuplicateChallengeReader), DuplicateSourceReader: reader.(searchstore.DuplicateSourceReader), fault: r.fault})
	})
}

func (r *nativeBrowserPathRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if ctx.Value(nativeDuplicateRecheckKey{}) == true {
		r.writes.Add(1)
		if before := r.beforeWrite; before != nil {
			r.beforeWrite = nil
			before()
		}
	}
	return r.delegate.WriteTenant(ctx, org, workload, func(writer tenantstore.Writer) error {
		return write(&nativeBrowserPathWriter{Writer: writer, WorkItemRelationshipBatchReader: writer.(tenantstore.WorkItemRelationshipBatchReader), DuplicateChallengeWriter: writer.(corestore.DuplicateChallengeWriter), DuplicateSourceWriter: writer.(searchstore.DuplicateSourceWriter), DuplicateSourceReader: writer.(searchstore.DuplicateSourceReader), fault: r.fault})
	})
}

func installNativeBrowserPathFault(t *testing.T, s *Service) *nativeBrowserPathFault {
	t.Helper()
	fault := &nativeBrowserPathFault{}
	original := s.config.TenantRepository
	admin := conversationTestAdmin(t, s)
	wrapped := &nativeBrowserPathRepository{Repository: s.database.postgres, delegate: original, fault: fault}
	var installed tenantstore.Repository = wrapped
	if authority, ok := original.(tenantauth.Reader); ok {
		installed = &nativeBrowserPathAuthorityRepository{nativeBrowserPathRepository: wrapped, authority: authority}
	}
	s.config.TenantRepository = installed
	projectRecordDatabases.Store(installed, admin)
	t.Cleanup(func() {
		s.tokenUseWork.Wait()
		s.config.TenantRepository = original
		projectRecordDatabases.Delete(installed)
	})
	return fault
}

func setNativeDuplicateChallengeFixture(t *testing.T, f nativeFixture, enabled bool) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.DuplicateChallenge = enabled
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func markNativeDuplicateSearchRebuildFixture(t *testing.T, f nativeFixture) {
	t.Helper()
	admin := conversationTestAdmin(t, f.service)
	if _, err := admin.ExecContext(t.Context(), "DELETE FROM search_postings p USING search_documents d WHERE p.organization_id=d.organization_id AND p.document_id=d.id AND d.organization_id=$1 AND d.project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "INSERT INTO search_scope_refresh(organization_id,project_id,work_item_id,archived,principal_id) VALUES($1,$2,'',false,'') ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET after_id=''", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
}

func TestNativeDuplicateMCP(t *testing.T) {
	for _, test := range []struct {
		name, transport string
		failBrowserPath bool
		rebuilding      bool
	}{
		{"stdio", "stdio", false, false},
		{"http", "http", false, false},
		{"stdio post-commit failure", "stdio", true, false},
		{"http post-commit failure", "http", true, false},
		{"stdio rebuilding", "stdio", false, true},
		{"http rebuilding", "http", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "duplicate-mcp")
			first := f.create(t, "Runner checkout deadlocks")
			for i := range 6 {
				f.create(t, "Runner checkout stalls "+strconv.Itoa(i))
			}
			setNativeDuplicateChallengeFixture(t, f, true)
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			authority, err := f.service.operatorCurrentAuthority(ctx, credential, string(f.project.OrganizationID))
			if err != nil {
				t.Fatal(err)
			}
			ctx = f.service.withOperatorCatalog(authority.BindContext(ctx), credential, string(f.project.OrganizationID))
			protocol := hostedContextProtocol(t, f.service, ctx, test.transport)
			call := func(name string, args map[string]any) json.RawMessage {
				t.Helper()
				args["project_id"] = f.project.ID
				reply := protocol("tools/call", name, args)
				var result struct {
					IsError bool `json:"isError"`
					Content struct {
						Data json.RawMessage `json:"data"`
					} `json:"structuredContent"`
				}
				if err := json.Unmarshal(reply.Result, &result); err != nil || result.IsError || len(reply.Error) != 0 {
					t.Fatalf("%s failed: %s %s, %v", name, reply.Result, reply.Error, err)
				}
				return result.Content.Data
			}
			args := map[string]any{"request_id": "mcp-duplicate", "title": first.Title, "description": "Private submitted description" + issueContractTestSections, "state": "Todo"}
			args["project_id"] = f.project.ID
			if test.rebuilding {
				if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
					t.Fatal(err)
				}
				markNativeDuplicateSearchRebuildFixture(t, f)
				raw := call("file_issue", args)
				var filed tracker.NativeIssue
				if err := json.Unmarshal(raw, &filed); err != nil || filed.WorkItemID == "" {
					t.Fatalf("filing during rebuild: %s err=%v", raw, err)
				}
				return
			}
			for _, test := range []struct{ name, state, body, message string }{
				{"unlinked Triage", "Triage", "Description" + issueContractTestSections, "Triage is reserved for linked GitHub issue intake"},
				{"unknown lane", "Unknown", "Description" + issueContractTestSections, "Workflow state does not exist"},
				{"missing issue contract", "Todo", "Description without required sections", "Add or complete these required issue sections:"},
				{"default lane contract", "", "Description without required sections", "Add or complete these required issue sections:"},
			} {
				t.Run(test.name, func(t *testing.T) {
					args["state"], args["description"] = test.state, test.body
					refused := protocol("tools/call", operatortool.FileIssue, args)
					var refusal struct {
						IsError bool `json:"isError"`
						Content struct {
							operatortool.RequestError
							duplicateChallenge
						} `json:"structuredContent"`
					}
					if err := json.Unmarshal(refused.Result, &refusal); err != nil || !refusal.IsError || len(refused.Error) != 0 || refusal.Content.Code != "invalid_request" || !strings.Contains(refusal.Content.Message, test.message) || refusal.Content.ChallengeToken != "" {
						t.Fatalf("lane refusal=%s %s err=%v", refused.Result, refused.Error, err)
					}
					var issues, challenges int
					if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&issues); err != nil {
						t.Fatal(err)
					}
					if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&challenges); err != nil {
						t.Fatal(err)
					}
					if issues != 7 || challenges != 0 {
						t.Fatalf("refused lane created issues=%d challenges=%d", issues, challenges)
					}
				})
			}
			args["description"] = "Private submitted description" + issueContractTestSections
			args["state"] = "Todo"
			raw := call(operatortool.FileIssue, args)
			var challenge duplicateChallenge
			if err := json.Unmarshal(raw, &challenge); err != nil {
				t.Fatal(err)
			}
			if challenge.ChallengeToken == "" || len(challenge.PossibleDuplicates) != 5 || challenge.PossibleDuplicates[0].WorkItemID != first.WorkItemID || strings.Contains(string(raw), "Full issue content") || strings.Contains(string(raw), "Private submitted description") {
				t.Fatalf("unranked, unbounded or content-leaking challenge: %s", raw)
			}
			var numbers []int
			for _, match := range challenge.PossibleDuplicates {
				numbers = append(numbers, match.Number)
			}
			args["challenge_token"], args["not_duplicate_of"] = challenge.ChallengeToken, numbers
			var fault *nativeBrowserPathFault
			if test.failBrowserPath {
				fault = installNativeBrowserPathFault(t, f.service)
				fault.enabled.Store(true)
			}
			raw = call(operatortool.FileIssue, args)
			var created tracker.NativeIssue
			if err := json.Unmarshal(raw, &created); err != nil || created.WorkItemID == "" {
				t.Fatalf("confirmation=%s: %v", raw, err)
			}
			if created.State != "Todo" || created.Body != args["description"] || created.Title != first.Title {
				t.Fatalf("confirmation lost committed item: %s", raw)
			}
			if fault != nil {
				if fault.failures.Load() != 0 || slices.Contains(created.OmittedFields, "web_url") || created.WebURL != "" {
					t.Fatalf("receipt performed a late browser path read: failures=%d response=%s", fault.failures.Load(), raw)
				}
				for _, failing := range []bool{true, false} {
					fault.enabled.Store(failing)
					raw = call(operatortool.FileIssue, args)
					var replay tracker.NativeIssue
					if err := json.Unmarshal(raw, &replay); err != nil || replay.WorkItemID != created.WorkItemID || slices.Contains(replay.OmittedFields, "web_url") || replay.WebURL != created.WebURL {
						t.Fatalf("replay after post-commit failure=%s: %v", raw, err)
					}
				}
				if fault.failures.Load() != 0 {
					t.Fatalf("replay performed a late browser path read: %d", fault.failures.Load())
				}
			}
			var count int
			if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != 8 {
				t.Fatalf("confirmation or replay duplicated item: count=%d err=%v", count, err)
			}
			raw = call(operatortool.WorkRelationships, map[string]any{"reference": created.WorkItemID})
			var links struct {
				Related []tracker.NativeDependency `json:"related"`
			}
			if err := json.Unmarshal(raw, &links); err != nil || len(links.Related) != 5 {
				t.Fatalf("related=%s: %v", raw, err)
			}
			read := func(id tracker.NativeWorkItemID) tracker.NativeIssue {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				return issue
			}
			for _, pair := range [][2]tracker.NativeWorkItemID{{first.WorkItemID, created.WorkItemID}, {created.WorkItemID, first.WorkItemID}} {
				current := read(pair[0])
				call(operatortool.SetDependency, map[string]any{"request_id": "remove-" + string(pair[0]), "identifier": pair[0], "related": pair[1], "expected_revision": strconv.FormatInt(int64(current.Revision), 10), "operation": "remove", "relationship_type": "related"})
			}
			current := read(first.WorkItemID)
			call(operatortool.SetDependency, map[string]any{"request_id": "manual-related", "identifier": first.WorkItemID, "related": created.WorkItemID, "expected_revision": strconv.FormatInt(int64(current.Revision), 10), "operation": "add", "relationship_type": "related"})
			raw = call(operatortool.WorkRelationships, map[string]any{"reference": first.WorkItemID})
			if err := json.Unmarshal(raw, &links); err != nil || len(links.Related) != 1 || links.Related[0].ID != created.WorkItemID {
				t.Fatalf("manual relationship=%s: %v", raw, err)
			}
		})
	}
}

func TestNativeDuplicateCandidateRecheck(t *testing.T) {
	for _, change := range []string{"new issue", "edited candidate", "archived candidate", "challenge enabled", "challenge disabled"} {
		t.Run(change, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			first := f.create(t, "Runner checkout deadlocks")
			setNativeDuplicateChallengeFixture(t, f, change != "challenge enabled")
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential, action: operatortool.FileIssue}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "candidate-recheck"}, Title: first.Title, Body: first.Body, State: "Todo"}
			search, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, request, false)
			if err != nil {
				t.Fatal(err)
			}
			if change != "challenge enabled" && len(search.matches) != 1 {
				t.Fatalf("matches=%+v", search.matches)
			}
			prepared, _, err := f.service.prepareWorkItemRecordMutation(t.Context(), scope, hubapi.NativeCommandOptions{OperationID: nativeIssueCreationOperation(scope), Feature: "collaboration"}, request.Mutation, request, "")
			if err != nil || prepared.write == nil {
				t.Fatalf("prepare current duplicate filing: %v", err)
			}
			switch change {
			case "new issue":
				f.createIndependent(t, "Separate mailbox failure")
			case "edited candidate":
				title := "Separate mailbox failure"
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(first.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-candidate"}, ExpectedRevision: first.Revision, Title: &title})
				requireNativeStatus(t, response, http.StatusOK)
			case "archived candidate":
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/archive", f.token, archiveRequest(first, "archive-candidate"))
				requireNativeStatus(t, response, http.StatusOK)
			case "challenge enabled", "challenge disabled":
				setNativeDuplicateChallengeFixture(t, f, change == "challenge enabled")
			}
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			err = f.service.config.TenantRepository.WriteTenant(t.Context(), scope.organization, corestore.Serving, prepared.write)
			if !errors.Is(err, corestore.ErrSerializationFailure) {
				t.Fatalf("stale duplicate preparation was accepted: %v", err)
			}
			if change == "archived candidate" || change == "challenge disabled" {
				raw, err := f.service.createNativeIssueCommand(t.Context(), scope, request)
				var challenge duplicateChallenge
				if err != nil || json.Unmarshal(raw, &challenge) != nil || challenge.ChallengeToken != "" || len(challenge.PossibleDuplicates) != 0 {
					t.Fatalf("challenge=%+v err=%v", challenge, err)
				}
				var created tracker.NativeIssue
				if json.Unmarshal(raw, &created) != nil || created.WorkItemID == "" || created.WorkItemID == first.WorkItemID {
					t.Fatalf("current canonical filing did not create independent work: %s", raw)
				}
			} else if change == "challenge enabled" {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
				requireNativeStatus(t, response, http.StatusConflict)
			}
		})
	}
	for _, change := range []string{"archived candidate", "challenge disabled"} {
		t.Run(change+" within one public call", func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			first := f.create(t, "Runner checkout deadlocks")
			setNativeDuplicateChallengeFixture(t, f, true)
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential, action: operatortool.FileIssue}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "same-call-recheck"}, Title: first.Title, Body: first.Body, State: "Todo"}
			installNativeBrowserPathFault(t, f.service)
			repository := f.service.config.TenantRepository.(*nativeBrowserPathRepository)
			repository.beforeWrite = func() {
				if change == "archived candidate" {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/archive", f.token, archiveRequest(first, "archive-candidate"))
					requireNativeStatus(t, response, http.StatusOK)
				} else {
					setNativeDuplicateChallengeFixture(t, f, false)
				}
				refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			}
			ctx := context.WithValue(t.Context(), nativeDuplicateRecheckKey{}, true)
			raw, err := f.service.createNativeIssueCommand(ctx, scope, request)
			var challenge duplicateChallenge
			if err != nil || json.Unmarshal(raw, &challenge) != nil || challenge.ChallengeToken != "" || len(challenge.PossibleDuplicates) != 0 {
				t.Fatalf("one-call challenge=%+v err=%v", challenge, err)
			}
			var created tracker.NativeIssue
			if json.Unmarshal(raw, &created) != nil || created.WorkItemID == "" || created.WorkItemID == first.WorkItemID || created.Title != first.Title || created.Body != first.Body {
				t.Fatalf("one-call filing did not preserve independent work: %s", raw)
			}
			if repository.writes.Load() != 2 || repository.beforeWrite != nil {
				t.Fatalf("one-call retry writes=%d pending mutation=%t", repository.writes.Load(), repository.beforeWrite != nil)
			}
			var items, challenges int
			admin := conversationTestAdmin(t, f.service)
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&items); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&challenges); err != nil {
				t.Fatal(err)
			}
			if items != 2 || challenges != 0 {
				t.Fatalf("one-call filing items=%d challenges=%d", items, challenges)
			}
		})
	}
}

func TestNativeMachineDuplicateCommentCandidate(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	owner := f.create(t, "Original failure")
	body := issueorigin.Stamp("Initial diagnostic"+issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: "original-failure"})
	path := f.base + "/work-items/" + string(owner.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "duplicate-owner-body"}, ExpectedRevision: owner.Revision, Body: &body}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "duplicate-owner-comment"}, Body: "internal/example/pool.go:23:4: undefined: capacity"}), http.StatusOK)
	refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	request := tracker.CreateIssue{Body: issueorigin.Stamp("internal/example/pool.go:31:2: undefined: capacity", issueorigin.Origin{Kind: "worker", Fingerprint: "later-failure"})}
	search, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, request, true)
	if err != nil || !search.available || len(search.matches) != 1 || search.matches[0].WorkItemID != owner.WorkItemID {
		t.Fatalf("search=%+v err=%v", search, err)
	}
}

func TestNativeDuplicatesIncompleteIndex(t *testing.T) {
	for _, state := range []string{"rebuild", "dirty"} {
		t.Run(state, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			body := issueorigin.Stamp("Matching diagnostic"+issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: "search-rebuild"})
			first := f.create(t, "Repository association deadlocks")
			path := f.base + "/work-items/" + string(first.WorkItemID)
			response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "source-body"}, ExpectedRevision: first.Revision, Body: &body})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &first)
			setNativeDuplicateChallengeFixture(t, f, true)
			refreshNativeDuplicateSearchFixture(t, f.service, f.project.OrganizationID)
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			request := tracker.CreateIssue{Title: first.Title, Body: body}
			for _, machine := range []bool{false, true} {
				search, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, request, machine)
				if err != nil || len(search.matches) != 1 {
					t.Fatalf("complete index machine=%v search=%v err=%v", machine, search, err)
				}
			}
			if state == "rebuild" {
				markNativeDuplicateSearchRebuildFixture(t, f)
			} else {
				labels := []string{"pending-index"}
				response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "dirty-index"}, ExpectedRevision: first.Revision, Labels: &labels})
				requireNativeStatus(t, response, http.StatusOK)
			}
			for _, machine := range []bool{false, true} {
				search, err := f.service.prepareWorkItemDuplicates(t.Context(), scope, request, machine)
				if err != nil || search.available || len(search.matches) != 0 {
					t.Fatalf("incomplete index machine=%v search=%v err=%v", machine, search, err)
				}
			}
			request.IdempotencyKey = "machine-file-during-rebuild"
			request.Body += issueContractTestSections
			request.State = "Todo"
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var filed tracker.NativeIssue
			decodeHubResponse(t, response, &filed)
			if filed.WorkItemID == "" || filed.WorkItemID == first.WorkItemID {
				t.Fatalf("incomplete index reused a duplicate: %s", response.Body.String())
			}
		})
	}
}
