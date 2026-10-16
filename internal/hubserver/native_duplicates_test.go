package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDuplicateFiling(t *testing.T) {
	for _, test := range []struct {
		name       string
		enabled    bool
		title      string
		body       string
		closedDays int
		challenge  bool
		archived   bool
	}{
		{"matching title", true, "Runner checkout deadlocks", "Separate observed details", 0, true, false},
		{"matching body", true, "A different symptom", "Checkout hangs during repository association", 0, true, false},
		{"no match", true, "Invitation email rejected", "Mailbox rejects the invite address; delivery returns an SMTP rejection from the recipient domain after verification links are sent. Sender reputation, relay credentials, mailbox quota, attachments, calendar invitations, spam filtering, and address formatting each need investigation.", 0, false, false},
		{"disabled", false, "Runner checkout deadlocks", "Checkout hangs during repository association", 0, false, false},
		{"recent closed", true, "Runner checkout deadlocks", "Separate observed details", 29, true, false},
		{"old closed", true, "Runner checkout deadlocks", "Separate observed details", 31, false, false},
		{"recent archived", true, "Runner checkout deadlocks", "Separate observed details", 29, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC).AddDate(0, 0, -test.closedDays)
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
			f := newNativeFixture(t, service, "", "duplicates")
			first := f.create(t, "Runner checkout deadlocks")
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body = ? WHERE native_id = ?", "Checkout hangs during repository association"+issueContractTestSections, first.WorkItemID); err != nil {
				t.Fatal(err)
			}
			foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign")
			foreign.create(t, test.title)
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge = ? WHERE id = ?", test.enabled, f.project.ID); err != nil {
				t.Fatal(err)
			}
			if test.closedDays != 0 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close"}, ExpectedRevision: first.Revision, State: "Done", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				now = now.AddDate(0, 0, test.closedDays)
			}
			if test.archived {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE native_id=?", first.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "filing"}, Title: test.title, Body: test.body + issueContractTestSections, State: "Todo"}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			wantStatus := http.StatusOK
			if test.challenge {
				wantStatus = http.StatusConflict
			}
			requireNativeStatus(t, response, wantStatus)
			var challenge duplicateChallenge
			decodeHubResponse(t, response, &challenge)
			if test.challenge {
				var failure apiErrorResponse
				decodeHubResponse(t, response, &failure)
				if failure.Code != "possible_duplicates" || failure.Message == "" {
					t.Fatalf("challenge omitted API error contract: %s", response.Body.String())
				}
			}
			if (challenge.ChallengeToken != "") != test.challenge {
				t.Fatalf("challenge = %s", response.Body.String())
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id = ?", f.project.ID).Scan(&count); err != nil {
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
			original := request
			request.ChallengeToken = challenge.ChallengeToken
			request.NotDuplicateOf = []int{first.Number}
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
			initialReplay := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, original)
			requireNativeStatus(t, initialReplay, http.StatusOK)
			if initialReplay.Body.String() != response.Body.String() {
				t.Fatal("original request did not return completed issue")
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
	otherIssue := other.create(t, "Independent project report")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/work-items/"+string(otherIssue.WorkItemID), other.token, nil), http.StatusOK)
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+credential.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": other.project.ID}), http.StatusNoContent)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge = 1 WHERE id = ?", f.project.ID); err != nil {
		t.Fatal(err)
	}
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
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	request.IdempotencyKey = "reuse"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request), http.StatusUnprocessableEntity)
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
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id = ?", f.project.ID).Scan(&count); err != nil {
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
		{"expiry boundary", time.Hour, http.StatusUnprocessableEntity},
		{"expired", time.Hour + time.Second, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
			f := newNativeFixture(t, service, "", "lifetime")
			first := f.create(t, "Runner checkout deadlocks")
			if _, err := service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=1 WHERE id=?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "lifetime"}, Title: first.Title, Body: "Independent description" + issueContractTestSections, State: "Todo"}
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			var challenge duplicateChallenge
			decodeHubResponse(t, response, &challenge)
			if challenge.ChallengeToken == "" {
				t.Fatalf("no challenge: %s", response.Body.String())
			}
			request.ChallengeToken = challenge.ChallengeToken
			now = now.Add(test.age)
			response = performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusUnprocessableEntity {
				ctx := changeOperatorContext(t, service, f.token, string(f.project.OrganizationID))
				credential, _, err := service.authenticateAPIToken(t.Context(), f.token, "", "")
				if err != nil {
					t.Fatal(err)
				}
				authority, err := service.operatorCurrentAuthority(ctx, credential, string(f.project.OrganizationID))
				if err != nil {
					t.Fatal(err)
				}
				ctx = service.withOperatorCatalog(authority.BindContext(ctx), credential, string(f.project.OrganizationID))
				raw, err := json.Marshal(map[string]any{"project_id": f.project.ID, "request_id": request.IdempotencyKey, "title": request.Title, "description": request.Body, "state": request.State, "challenge_token": request.ChallengeToken})
				if err != nil {
					t.Fatal(err)
				}
				_, err = (hostedOperatorExecutor{service}).Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				var refusal *operatortool.RequestError
				if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "Review fresh suggestions") {
					t.Fatalf("expiry was not actionable: %v", err)
				}
			}

			var count int
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project.ID).Scan(&count); err != nil {
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
				if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE token=?", challenge.ChallengeToken).Scan(&count); err != nil || count != 0 {
					t.Fatalf("consumed challenge retained: %d, %v", count, err)
				}
			}
			if err := service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM native_duplicate_challenges WHERE token=?", challenge.ChallengeToken).Scan(&count); err != nil || count != 0 {
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

func TestNativeDuplicateMCP(t *testing.T) {
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "duplicate-mcp")
			first := f.create(t, "Runner checkout deadlocks")
			for i := range 6 {
				f.create(t, "Runner checkout stalls "+strconv.Itoa(i))
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=1 WHERE id=?", f.project.ID); err != nil {
				t.Fatal(err)
			}
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
			protocol := hostedContextProtocol(t, f.service, ctx, transport)
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
			raw = call(operatortool.FileIssue, args)
			var created tracker.NativeIssue
			if err := json.Unmarshal(raw, &created); err != nil || created.WorkItemID == "" {
				t.Fatalf("confirmation=%s: %v", raw, err)
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

func TestHostedDuplicateConfirmation(t *testing.T) {
	for _, deployment := range []string{"shared", "dedicated"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "member")
			executor := hostedOperatorExecutor{f.service}
			call := func(args map[string]any) tracker.NativeIssue {
				t.Helper()
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Data json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &envelope); err != nil {
					t.Fatal(err)
				}
				var challenge duplicateChallenge
				if err := json.Unmarshal(envelope.Data, &challenge); err != nil {
					t.Fatal(err)
				}
				if challenge.ChallengeToken != "" {
					args["challenge_token"] = challenge.ChallengeToken
					if args["request_id"] != "filing-1" {
						numbers := []int{}
						for _, match := range challenge.PossibleDuplicates {
							numbers = append(numbers, match.Number)
						}
						args["not_duplicate_of"] = numbers
					}
					raw, err = json.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					if args["request_id"] == "filing-1" {
						if _, err := f.service.database.db.ExecContext(ctx, `CREATE TRIGGER fail_confirm BEFORE INSERT ON issues BEGIN SELECT RAISE(ABORT, 'injected issue write failure'); END`); err != nil {
							t.Fatal(err)
						}
						if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw}); err == nil {
							t.Fatal("injected write failure succeeded")
						}
						if _, err := f.service.database.db.ExecContext(ctx, "DROP TRIGGER fail_confirm"); err != nil {
							t.Fatal(err)
						}
					}
					result, err = executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(result.Content, &envelope); err != nil {
						t.Fatal(err)
					}
				}
				var issue tracker.NativeIssue
				if err := json.Unmarshal(envelope.Data, &issue); err != nil || issue.WorkItemID == "" {
					t.Fatalf("issue=%s, err=%v", result.Content, err)
				}
				return issue
			}
			for i := range 3 {
				args := map[string]any{"project_id": f.project, "request_id": "filing-" + strconv.Itoa(i), "title": "Runner checkout deadlocks", "description": "Independent observation" + issueContractTestSections, "priority": 2}
				original := make(map[string]any, len(args))
				for key, value := range args {
					original[key] = value
				}
				issue := call(args)
				replay := call(args)
				initialReplay := call(original)
				if initialReplay.WorkItemID != issue.WorkItemID {
					t.Fatal("original replay changed the created issue")
				}
				raw, err := json.Marshal(map[string]any{"project_id": f.project, "reference": issue.WorkItemID})
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkItem, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				var readback struct {
					Data tracker.NativeIssue `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &readback); err != nil || readback.Data.WorkItemID != issue.WorkItemID || readback.Data.Priority == nil || *readback.Data.Priority != 1 {
					t.Fatalf("readback=%s err=%v", result.Content, err)
				}
				if replay.WorkItemID != issue.WorkItemID || issue.Priority == nil || *issue.Priority != 1 {
					t.Fatalf("issue=%#v, replay=%#v", issue, replay)
				}
			}
			var count, related int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", f.project).Scan(&count); err != nil || count != 3 {
				t.Fatalf("issue count=%d err=%v", count, err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_relationships").Scan(&related); err != nil || related != 2 {
				t.Fatalf("relationship count=%d err=%v", related, err)
			}

			t.Run("same principal across credentials", func(t *testing.T) {
				resolve := ctx.Value(nativeOperatorScopeKey{}).(func(context.Context) (nativeScope, error))
				issuer, err := resolve(ctx)
				if err != nil {
					t.Fatal(err)
				}
				issuer.project = f.project
				expires := f.service.config.now().Add(time.Hour)
				key, err := f.service.createAPITokenFor(ctx, tokenRequest{Name: "confirmation-client", Scope: apiScopeOperator, Issuer: &issuer.credential, KeyScope: apikey.ScopeWrite, ExpiresAt: &expires, ProjectIDs: []string{string(f.project)}, ProjectAccess: hostedProjectsSelected})
				if err != nil {
					t.Fatal(err)
				}
				credential, _, err := f.service.authenticateAPIToken(ctx, key.Token, "", "")
				if err != nil {
					t.Fatal(err)
				}
				confirmer := nativeScope{organization: issuer.organization, project: f.project, credential: credential}
				if issuer.credential.ID == credential.ID || issuer.actor().PrincipalID != confirmer.actor().PrincipalID {
					t.Fatal("fixture requires distinct credentials for the same principal")
				}
				keyCtx := changeOperatorContext(t, f.service, key.Token, string(issuer.organization))
				authority, err := f.service.operatorCurrentAuthority(keyCtx, credential, string(issuer.organization))
				if err != nil {
					t.Fatal(err)
				}
				keyCtx = f.service.withOperatorCatalog(authority.BindContext(keyCtx), credential, string(issuer.organization))
				invoke := func(current context.Context, input map[string]any) json.RawMessage {
					t.Helper()
					arguments, err := json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
					result, err := executor.Execute(current, operatortool.Call{Name: operatortool.FileIssue, Arguments: arguments})
					if err != nil {
						t.Fatalf("filing: %v", err)
					}
					var envelope struct {
						Data json.RawMessage `json:"data"`
					}
					if err := json.Unmarshal(result.Content, &envelope); err != nil {
						t.Fatal(err)
					}
					return envelope.Data
				}
				draft := map[string]any{"project_id": f.project, "request_id": "cross-credential", "title": "Runner checkout deadlocks", "description": "Separate observation" + issueContractTestSections, "state": "Todo", "priority": 2}
				raw := invoke(ctx, draft)
				var challenge duplicateChallenge
				if err := json.Unmarshal(raw, &challenge); err != nil || challenge.ChallengeToken == "" {
					t.Fatalf("challenge=%s err=%v", raw, err)
				}
				confirmed := maps.Clone(draft)
				confirmed["challenge_token"] = challenge.ChallengeToken
				raw = invoke(keyCtx, confirmed)
				var created tracker.NativeIssue
				if err := json.Unmarshal(raw, &created); err != nil || created.WorkItemID == "" {
					t.Fatalf("created=%s err=%v", raw, err)
				}
				for i, input := range []map[string]any{draft, confirmed} {
					current := ctx
					if i == 1 {
						current = keyCtx
					}
					raw := invoke(current, input)
					var issue tracker.NativeIssue
					if json.Unmarshal(raw, &issue) != nil || issue.WorkItemID != created.WorkItemID {
						t.Fatalf("completed replay=%s", raw)
					}
				}
				if err := f.service.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM issues WHERE project_id=?", f.project).Scan(&count); err != nil || count != 4 {
					t.Fatalf("issue count=%d err=%v", count, err)
				}
			})
		})
	}
}
