package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeReleaseStabilizationOccurrences(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
	if err != nil {
		t.Fatal(err)
	}
	selected := tracker.ProjectID(issueorigin.DetentCloudProjectID)
	for _, statement := range []string{
		"INSERT INTO projects (id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled) SELECT ?, organization_id, 'release-reporting', profile, states_json, require_dependencies, created_at, github_repository_enabled FROM projects WHERE id = ?",
		"INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) SELECT ?, source_name, detent_state, terminal, dispatchable, created_at, updated_at FROM workflow_states WHERE project_id = ?",
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement, selected, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []tracker.ProjectID{selected, f.project.ID} {
		scope := nativeScope{organization: f.project.OrganizationID, project: project, credential: credential}
		seedNativeFixturePolicy(t, f.service, string(scope.organization)+"/"+string(project))
		file := func(series, key string) tracker.NativeIssue {
			t.Helper()
			body := "Problem: `go-test:example/pkg:TestOne`\n" + issueContractTestSections
			origin := issueorigin.Origin{Kind: "worker", Instance: "runner", Source: key, Fingerprint: issueorigin.Fingerprint("go-test:example/pkg:TestOne")}
			if series != "" {
				body += "\nRelease stabilization: `" + series + "`\n"
				origin = issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "https://github.com/digitaldrywood/detent/actions/runs/1/attempts/1", Fingerprint: issueorigin.ReleaseStabilizationFingerprint(series)}
			}
			request := tracker.CreateIssue{Title: "Scheduled repair", Body: issueorigin.Stamp(body, origin), State: "Todo", Priority: new(1)}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			issue, err := reportNativeMachineIssueTx(t.Context(), tx, scope, request, f.service.config.now())
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			return issue
		}
		repair := file("", "repair")
		first := file("release/v1.2", "first")
		second := file("release/v1.3", "second")
		replayed := file("release/v1.2", "later")
		worker := file("", "another-worker")
		if project == selected {
			if repair.WorkItemID == first.WorkItemID || first.WorkItemID == second.WorkItemID || replayed.WorkItemID != first.WorkItemID || !replayed.PublicationReused || worker.WorkItemID != repair.WorkItemID {
				t.Fatal("release series reused another repair or failed to reuse its own native owner")
			}
		} else if first.WorkItemID != repair.WorkItemID || second.WorkItemID != repair.WorkItemID || replayed.WorkItemID != repair.WorkItemID || worker.WorkItemID != repair.WorkItemID {
			t.Fatal("release-specific identity changed another project's defect matching")
		}
	}
}

func TestNativeMachineDefectOccurrences(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		first          string
		second         string
		otherProject   bool
		state          string
		scheduledFirst bool
		disabled       bool
		legacyBody     string
		endState       string
		newEpisode     bool
	}{
		{"test failure", "Gate failed:\n--- FAIL: TestHostedChangePolicyJourney (0.03s)\nFAIL github.com/digitaldrywood/detent/internal/hubserver 0.04s", "Another worker found:\n--- FAIL: TestHostedChangePolicyJourney (0.08s)\nFAIL github.com/digitaldrywood/detent/internal/hubserver 0.09s", false, "Todo", false, false, "", "Done", true},
		{"migration collision", "panic: goose: duplicate version 20261006023000 detected:\nsprite_placement.sql", "Gate blocked by duplicate migration version 20261006023000: github_references.go and sprite_placement.sql", false, "Todo", false, false, "", "Done", true},
		{"compiler diagnostic", "internal/hub/pool.go:23:4: undefined: capacity", "internal/hub/pool.go:31:2: undefined: capacity", false, "Todo", false, false, "", "Done", true},
		{"scheduled owner", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", false, "Todo", true, false, "", "Done", true},
		{"other project policy", "goose: duplicate version 20261006023000 detected:", "duplicate migration version 20261006023000", true, "Backlog", false, false, "", "Done", true},
		{"disabled challenge", "internal/example/check.go:23:4: undefined: capacity", "internal/example/check.go:31:2: undefined: capacity", false, "Todo", false, true, "", "Done", true},
		{"incomplete legacy Todo", "internal/example/legacy.go:23:4: undefined: capacity", "internal/example/legacy.go:31:2: undefined: capacity", false, "Todo", false, false, "## Roadmap Summary\nRepair the failed build.", "Done", true},
		{"held legacy Todo", "internal/example/held.go:23:4: undefined: capacity", "internal/example/held.go:31:2: undefined: capacity", false, "Todo", false, false, "## Roadmap Summary\nRepair the failed build.\n<!-- detent:no-dispatch -->", "Done", false},
		{"other project scheduled owner", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", true, "Todo", true, false, "", "Done", true},
		{"other project custom completion", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", true, "Todo", true, false, "", "Shipped", true},
		{"other project held custom completion", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", true, "Todo", true, false, "Problem: `go-test:example/hub:TestJourney`\n## Roadmap Summary\nRepair the failed build.\n<!-- detent:no-dispatch -->", "Shipped", false},
		{"other project cancelled", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", true, "Todo", true, false, "", "Cancelled", false},
		{"other project held", "Problem: `go-test:example/hub:TestJourney`", "--- FAIL: TestJourney (0.01s)\nFAIL example/hub 0.02s", true, "Todo", true, false, "Problem: `go-test:example/hub:TestJourney`\n## Roadmap Summary\nRepair the failed build.\n<!-- detent:no-dispatch -->", "Done", false},
	} {
		t.Run("typed intake/"+test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			states := append(f.project.States, tracker.NativeState{Name: "Backlog"}, tracker.NativeState{Name: "Cancelled", Terminal: true}, tracker.NativeState{Name: "Shipped", Terminal: true})
			for i := range states {
				if states[i].Name == "Todo" || states[i].Name == "Backlog" {
					states[i].Transitions = []string{"Done", "Cancelled", "Shipped"}
				}
			}
			project := f.project.ID
			if !test.otherProject {
				project = tracker.ProjectID(issueorigin.DetentCloudProjectID)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				record.States = states
				record.Settings.DuplicateChallenge = !test.disabled
				if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
					return err
				}
				if project != f.project.ID {
					_, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.project.OrganizationID, ID: project, Name: "defect-reporting", Profile: "native"}, States: states, Settings: projectstore.Settings{DuplicateChallenge: !test.disabled}})
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: project, credential: credential}
			seedNativeFixturePolicy(t, f.service, string(scope.organization)+"/"+string(project))
			base := "/api/v2/organizations/" + string(scope.organization) + "/projects/" + string(project)
			intake := func(request tracker.CreateIssue) tracker.NativeIssue {
				t.Helper()
				request, err := normalizeNativeMachineIntake(scope, states, request)
				if err != nil {
					t.Fatal(err)
				}
				if request.IdempotencyKey == "" {
					request.IdempotencyKey = newNativeID("machine-report")
				}
				raw, err := f.service.executeWorkItemRecordMutation(t.Context(), scope, nativeCommandOptions{OperationID: "machine-defect-fixture", Feature: "collaboration"}, request.Mutation, request, "")
				if err != nil {
					t.Fatal(err)
				}
				var issue tracker.NativeIssue
				if err := json.Unmarshal(raw, &issue); err != nil {
					t.Fatal(err)
				}
				operatorRefreshSearch(t, f)
				if err := f.service.refreshBoardRecord(t.Context(), scope.organization, viewstore.BoardCandidate{ProjectID: project, WorkItemID: issue.WorkItemID}, f.service.config.now()); err != nil {
					t.Fatal(err)
				}
				return issue
			}
			scope.action = operatortool.FileIssue
			admin := nativeAttemptTestDatabase(t, f)
			var owner tracker.NativeIssue
			for i, evidence := range []string{test.first, test.second} {
				origin := issueorigin.Origin{Kind: "worker", Instance: "runner-" + strconv.Itoa(i), Source: "attempt-" + strconv.Itoa(i), Fingerprint: "invented-" + strconv.Itoa(i)}
				if test.scheduledFirst && i == 0 {
					origin.Kind = "doctor"
					origin.Instance = "github-actions"
				}
				request := tracker.CreateIssue{Title: "Different wording " + strconv.Itoa(i), Body: issueorigin.Stamp(evidence+issueContractTestSections, origin), State: "Backlog", Priority: new(3)}
				if test.scheduledFirst && i == 0 {
					request.State = "Todo"
					request.IdempotencyKey = "first-scheduled-failure"
				}
				issue := intake(request)
				if i == 0 {
					owner = issue
					if test.legacyBody != "" {
						previous, _ := issueorigin.Parse(owner.Body)
						owner.Body = issueorigin.Stamp(test.legacyBody, previous)
						response := performHubAPIRequest(t, f.service, http.MethodPatch, base+"/work-items/"+string(owner.WorkItemID), testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "legacy-body"}, ExpectedRevision: owner.Revision, Body: &owner.Body})
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &owner)
						operatorRefreshSearch(t, f)
					}
				} else if issue.WorkItemID != owner.WorkItemID || !issue.PublicationReused {
					t.Fatalf("duplicate report created a second issue: %#v", issue)
				}
			}
			wantPriority := 1
			if test.otherProject {
				wantPriority = 3
			}
			if owner.State != test.state || owner.Priority == nil || *owner.Priority != wantPriority {
				t.Fatalf("owner = %#v, want %s priority %d", owner, test.state, wantPriority)
			}
			var issues, comments int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", scope.organization, project).Scan(&issues); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", scope.organization, project, owner.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if issues != 1 || comments != 1 {
				t.Fatalf("issues=%d comments=%d, want one issue and one occurrence", issues, comments)
			}
			fingerprint := issueorigin.DefectFingerprint(test.first)
			response := performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items?open=true&fingerprint="+fingerprint, testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page tracker.Page[tracker.NativeIssue]
			decodeHubResponse(t, response, &page)
			if len(page.Items) != 1 || page.Items[0].WorkItemID != owner.WorkItemID {
				t.Fatalf("bounded fingerprint search = %#v", page)
			}
			origin := issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "scheduled-run", Fingerprint: "scheduled-wording"}
			if test.scheduledFirst {
				previous, _ := issueorigin.Parse(owner.Body)
				origin.Fingerprint = previous.Fingerprint
			}
			scheduled := intake(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "scheduled-occurrence"}, Title: "Scheduled failure", Body: issueorigin.Stamp(test.second+issueContractTestSections, origin), State: "Todo", Priority: new(1)})
			if scheduled.WorkItemID != owner.WorkItemID || scheduled.State != owner.State || !scheduled.PublicationReused || scheduled.Body != owner.Body {
				t.Fatalf("scheduled report did not retain worker owner: %#v", scheduled)
			}
			if test.legacyBody != "" {
				response := performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items/"+string(owner.WorkItemID)+"/comments", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var page tracker.Page[tracker.NativeComment]
				decodeHubResponse(t, response, &page)
				var occurrence string
				for _, comment := range page.Items {
					if strings.Contains(comment.Body, "scheduled-run") {
						occurrence = comment.Body
					}
				}
				if !strings.Contains(occurrence, test.second) {
					t.Fatalf("scheduled occurrence lost failure evidence: %q", occurrence)
				}
				approval, err := f.service.readTenantProjectPolicy(t.Context(), scope.organization, project)
				if err != nil {
					t.Fatal(err)
				}
				contract, err := config.ResolvePolicyIssueContract(approval.Policy)
				if err != nil {
					t.Fatal(err)
				}
				evaluation := contract.Evaluate(nativeContractIssue(scheduled))
				if nativeClaimIssueContractAllowed(nativeClaimPlan{issue: scheduled, descriptor: approval.Policy}) || evaluation.Satisfied() || len(evaluation.Missing) == 0 {
					t.Fatalf("occurrence admitted incomplete issue: %+v", evaluation)
				}
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/work-items/"+string(owner.WorkItemID)+"/workflow", testHubAdminToken, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "finish-repair"}, ExpectedRevision: scheduled.Revision, State: test.endState, Reason: "user_requested"})
			requireNativeStatus(t, response, http.StatusOK)
			operatorRefreshSearch(t, f)
			request := tracker.CreateIssue{Title: "Late report", State: "Backlog", Priority: new(0), Body: issueorigin.Stamp(test.second, issueorigin.Origin{Kind: "worker", Source: "late-attempt", Fingerprint: "late-wording"})}
			closed := intake(request)
			if closed.WorkItemID != owner.WorkItemID || closed.State != test.endState || !closed.Terminal || !closed.PublicationReused || closed.Priority == nil || *closed.Priority != 1 {
				t.Fatalf("terminal report changed handled owner: %#v", closed)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", scope.organization, project, owner.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if comments != 2 {
				t.Fatalf("terminal report added an occurrence: %d", comments)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items/"+string(owner.WorkItemID), testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var historicalBefore tracker.NativeIssue
			decodeHubResponse(t, response, &historicalBefore)
			origin.Source = "new-scheduled-run"
			request = tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "new-scheduled-occurrence"}, Title: "Recurring scheduled failure", Body: issueorigin.Stamp(test.second+issueContractTestSections, origin), State: "Todo", Priority: new(1)}
			var current tracker.NativeIssue
			for replay := range 2 {
				reported := intake(request)
				if replay == 0 {
					current = reported
				} else if !reflect.DeepEqual(reported, current) {
					t.Fatalf("scheduled replay changed result: %#v, want %#v", reported, current)
				}
			}
			if !test.newEpisode {
				if current.WorkItemID != owner.WorkItemID || current.State != test.endState || !current.Terminal || !current.PublicationReused {
					t.Fatalf("scheduled report overrode a hold or cancellation: %#v", current)
				}
			} else {
				if current.WorkItemID == owner.WorkItemID || current.Terminal || current.PublicationReused || current.State != "Todo" || current.Priority == nil || *current.Priority != 1 || current.Body != request.Body {
					t.Fatalf("scheduled report did not retain a fresh occurrence: %#v", current)
				}
				if current.IssueContract != nil && (current.IssueContract.Exempt || len(current.IssueContract.ConfirmedSections) != 0) {
					t.Fatalf("scheduled report granted contract approval: %#v", current.IssueContract)
				}
				origin.Source = "following-scheduled-run"
				request.Body = issueorigin.Stamp(test.second, origin)
				request.IdempotencyKey = "following-scheduled-occurrence"
				reused := intake(request)
				if reused.WorkItemID != current.WorkItemID || !reused.PublicationReused || reused.Body != current.Body {
					t.Fatalf("following occurrence did not reuse open owner: %#v", reused)
				}
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", scope.organization, project, current.WorkItemID).Scan(&comments); err != nil {
					t.Fatal(err)
				}
				if comments != 1 {
					t.Fatalf("fresh owner comments = %d, want one following occurrence", comments)
				}
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, base+"/work-items/"+string(owner.WorkItemID), testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var historical tracker.NativeIssue
			decodeHubResponse(t, response, &historical)
			if historical.ChangeSequence < historicalBefore.ChangeSequence {
				t.Fatal("scheduled report regressed project change sequence")
			}
			historicalBefore.ChangeSequence = historical.ChangeSequence
			if !reflect.DeepEqual(historical, historicalBefore) {
				t.Fatalf("scheduled report changed terminal history: %#v, want %#v", historical, historicalBefore)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", scope.organization, project, owner.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if comments != 2 {
				t.Fatalf("scheduled report changed terminal comments: %d", comments)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", scope.organization, project).Scan(&issues); err != nil {
				t.Fatal(err)
			}
			wantIssues := 2
			if !test.newEpisode {
				wantIssues = 1
			}
			if issues != wantIssues {
				t.Fatalf("issues = %d, want %d", issues, wantIssues)
			}

		})
	}
	for _, test := range []struct {
		name              string
		evidence          bool
		verifier          bool
		delegation        bool
		rejected          bool
		mismatchedAttempt bool
		admitted          bool
		priority          int
	}{
		{name: "marker without evidence"},
		{name: "evidence without verifier", evidence: true, delegation: true},
		{name: "verifier without authored delegation", evidence: true, verifier: true},
		{name: "unverified run", evidence: true, verifier: true, delegation: true, rejected: true},
		{name: "mismatched run attempt", evidence: true, verifier: true, delegation: true, mismatchedAttempt: true},
		{name: "verified High", evidence: true, verifier: true, delegation: true, admitted: true, priority: 1},
		{name: "verified Urgent", evidence: true, verifier: true, delegation: true, admitted: true},
	} {
		t.Run("scheduled API/"+test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			existing := f.create(t, "Unchanged owner")
			const repository = "example-org/example-project"
			const criteria = "## Acceptance criteria\nComplete the requested work.\n\n## Must not break\nExisting behavior.\n\n## How we know it worked\nRun the project checks."
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.ProjectRecord(t.Context(), f.project.ID)
				if err != nil {
					return err
				}
				record.Settings.CheckoutRepository = repository
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if test.delegation {
				priority := "Urgent"
				if test.priority == 1 {
					priority = "High"
				}
				prompt := "## Scheduled CI repair contract\nNew verified GitHub Actions CI repair reports from `.github/workflows/verify.yaml` on `release/v*` enter Todo with " + priority + " priority.\n" + strings.ReplaceAll(criteria, "## ", "### ")
				workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte(prompt), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: " + repository + "\n  lanes:\n    - {name: Todo, role: active}\n    - {name: Blocked, role: holding}\n    - {name: Done, role: terminal}\n"), HasConfig: true, ConfigPath: "detent.yaml"})
				if err != nil {
					t.Fatal(err)
				}
				workflow.Definition.Revision = strings.Repeat("a", 40)
				descriptor, err := config.ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			}
			evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("b", 64), RunID: "123", RunAttempt: "2", JobID: "456", JobName: "Verify", RunURL: "https://github.com/" + repository + "/actions/runs/123/attempts/2", JobURL: "https://github.com/" + repository + "/actions/jobs/456", HeadSHA: strings.Repeat("a", 40), Conclusion: "failure"}
			origin := issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "scheduled-run", Fingerprint: issueorigin.Fingerprint("go-test:example/hub:TestJourney")}
			if test.evidence {
				origin.Source = evidence.RunURL
			}
			if test.mismatchedAttempt {
				origin.Source = "https://github.com/" + repository + "/actions/runs/123/attempts/1"
			}
			observations := 0
			if test.verifier {
				f.service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, request GitHubImportRequest) (GitHubImportPage, error) {
					observations++
					if request.Stage != "scheduled" || request.Repository != repository || request.ScheduledWorkflow != ".github/workflows/verify.yaml" || request.ScheduledBranch != "release/v*" || request.Scheduled == nil || !reflect.DeepEqual(*request.Scheduled, evidence) {
						t.Fatalf("scheduled observation=%+v", request)
					}
					if test.rejected {
						return GitHubImportPage{}, nil
					}
					return GitHubImportPage{Scheduled: &evidence}, nil
				})
			}
			diagnostic := "Problem: `go-test:example/hub:TestJourney`\n<unexpected>"
			body := issueorigin.Stamp(diagnostic, origin)
			if test.evidence {
				body += evidence.Stamp()
			}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "scheduled-api"}, Title: "Scheduled failure", Body: body, State: "Todo", Priority: new(3)}
			admin := nativeAttemptTestDatabase(t, f)
			snapshot := func() (tracker.NativeIssue, int, int) {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(existing.WorkItemID), f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				var issues, comments int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&issues); err != nil {
					t.Fatal(err)
				}
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&comments); err != nil {
					t.Fatal(err)
				}
				return issue, issues, comments
			}
			if !test.admitted {
				before, issueCount, commentCount := snapshot()
				for index, token := range []string{f.token, testHubAdminToken} {
					request.IdempotencyKey = fmt.Sprintf("unverified-%d", index)
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", token, request)
					requireNativeStatus(t, response, http.StatusUnprocessableEntity)
					after, issues, comments := snapshot()
					if !reflect.DeepEqual(before, after) || issues != issueCount || comments != commentCount {
						t.Fatalf("unverified report mutated owner/confirmation or counts: before=%+v after=%+v issues=%d/%d comments=%d/%d", before, after, issues, issueCount, comments, commentCount)
					}
				}
				return
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var owner tracker.NativeIssue
			decodeHubResponse(t, response, &owner)
			if observations != 1 || owner.State != "Todo" || owner.Terminal || owner.Priority == nil || *owner.Priority != test.priority || owner.IssueContract == nil || owner.IssueContract.RecordedAt.IsZero() || !reflect.DeepEqual(owner.IssueContract.ConfirmedSections, config.IssueContractSectionDigests(criteria)) {
				t.Fatalf("verified admission=%+v observations=%d", owner, observations)
			}
			if owner.Body == request.Body || !strings.HasPrefix(owner.Body, criteria+"\n\n## Verified CI occurrence") || !strings.Contains(owner.Body, "> &lt;unexpected&gt;") {
				t.Fatalf("verified body lost approved contract or quoted diagnostics: %s", owner.Body)
			}
			if got, found := issueorigin.Parse(owner.Body); !found || got != origin {
				t.Fatalf("verified origin=%+v want=%+v", got, origin)
			}
			operatorRefreshSearch(t, f)
			request.IdempotencyKey = "verified-occurrence"
			request.Body = issueorigin.Stamp(diagnostic+"\nAnother observation.", origin) + evidence.Stamp()
			var replayed tracker.NativeIssue
			for replay := range 2 {
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
				requireNativeStatus(t, response, http.StatusOK)
				var reused tracker.NativeIssue
				decodeHubResponse(t, response, &reused)
				if reused.WorkItemID != owner.WorkItemID || !reused.PublicationReused || reused.Body != owner.Body || reused.State != owner.State || reused.Priority == nil || *reused.Priority != *owner.Priority || !reflect.DeepEqual(reused.IssueContract.ConfirmedSections, owner.IssueContract.ConfirmedSections) {
					t.Fatalf("verified occurrence changed owner=%+v", reused)
				}
				if replay == 0 {
					replayed = reused
				} else if !reflect.DeepEqual(replayed, reused) {
					t.Fatalf("verified replay changed result=%+v want=%+v", reused, replayed)
				}
			}
			_, issues, comments := snapshot()
			if issues != 2 || comments != 1 {
				t.Fatalf("verified issue/occurrence counts=%d/%d want2/1", issues, comments)
			}
		})
	}
	for _, change := range []string{"revision", "archive"} {
		t.Run("fresh episode fencing/"+change, func(t *testing.T) {
			f := newPostgresDefaultNativeFixture(t, Config{})
			if serving, _ := f.service.database.postgres.WorkloadCapacities(); serving < 2 {
				t.Fatal("candidate mutation fixture requires two serving transactions")
			}
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential, action: operatortool.FileIssue}
			origin := issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: "completed-run", Fingerprint: issueorigin.Fingerprint("go-test:example/hub:TestFence")}
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "fence-owner"}, Title: "Completed owner", Body: issueorigin.Stamp("Problem: `go-test:example/hub:TestFence`"+issueContractTestSections, origin), State: "Done", Priority: new(1)}
			options := nativeCommandOptions{OperationID: "machine-defect-fixture", Feature: "collaboration"}
			raw, err := f.service.executeWorkItemRecordMutation(t.Context(), scope, options, request.Mutation, request, "")
			if err != nil {
				t.Fatal(err)
			}
			var owner tracker.NativeIssue
			if err := json.Unmarshal(raw, &owner); err != nil {
				t.Fatal(err)
			}
			if !owner.Terminal || owner.State != "Done" {
				t.Fatalf("fence owner=%+v", owner)
			}
			operatorRefreshSearch(t, f)
			admin := nativeAttemptTestDatabase(t, f)
			repository := f.service.config.TenantRepository
			mutated := false
			f.service.config.TenantRepository = createProofRepository{Repository: repository, beforeLock: func() {
				if mutated {
					return
				}
				mutated = true
				var response *httptest.ResponseRecorder
				if change == "archive" {
					response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(owner.WorkItemID)+"/archive", f.token, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "candidate-archive"}, ExpectedRevision: owner.Revision})
				} else {
					response = performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(owner.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "candidate-revision"}, ExpectedRevision: owner.Revision, Title: new("Changed completed owner")})
				}
				requireNativeStatus(t, response, http.StatusOK)
			}}
			defer func() { f.service.config.TenantRepository = repository }()
			origin.Source = "new-run"
			request.IdempotencyKey, request.State = "fence-new-episode", "Todo"
			request.Body = issueorigin.Stamp("Problem: `go-test:example/hub:TestFence`"+issueContractTestSections, origin)
			_, err = f.service.executeWorkItemRecordMutation(t.Context(), scope, options, request.Mutation, request, "")
			f.service.config.TenantRepository = repository
			var conflict *nativeError
			if !mutated || !errors.As(err, &conflict) || conflict.status != http.StatusConflict {
				t.Fatalf("candidate mutation was not fenced: mutated=%t error=%v", mutated, err)
			}
			if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), scope.project, owner.WorkItemID)
				if err != nil {
					return err
				}
				if row.Revision <= int64(owner.Revision) || row.Archived != (change == "archive") {
					t.Fatalf("candidate mutation did not commit: %+v", row)
				}
				if _, err := r.Receipt(t.Context(), credential.ID, options.OperationID, request.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("fenced fresh creation stored a receipt: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var issues, comments int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id=$2", scope.organization, scope.project).Scan(&issues); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE organization_id=$1 AND project_id=$2", scope.organization, scope.project).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			if issues != 1 || comments != 0 {
				t.Fatalf("fenced fresh creation wrote issue/occurrence counts=%d/%d want1/0", issues, comments)
			}
		})
	}
}
