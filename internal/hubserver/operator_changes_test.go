package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/logging"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func changeOperatorContext(t *testing.T, s *Service, token, organization string) context.Context {
	t.Helper()
	credential, _, err := s.authenticateAPIToken(t.Context(), token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	connection := operatortool.Connection{ID: "test-connection", Client: "test-client", Identity: operatorIdentity(credential, organization), Resolve: func(ctx context.Context) (operatortool.Authority, error) {
		current, _, err := s.authenticateAPIToken(ctx, token, "", "")
		if err != nil {
			return operatortool.Authority{}, err
		}
		return s.operatorCurrentAuthority(ctx, current, organization)
	}}
	return operatortool.WithConnection(t.Context(), connection)
}
func changeToolCall(name string, args operatortool.ChangeArguments) operatortool.Call {
	raw, _ := json.Marshal(args)
	return operatortool.Call{Name: name, Arguments: raw}
}

func TestOperatorChangePublication(t *testing.T) {
	for _, test := range []struct {
		name           string
		review, checks bool
		availability   string
		lane, status   string
	}{
		{"automatic promotion", false, false, "available", "Merging", "reviewed"},
		{"unverified artifacts", false, false, "unverified", "Merging", "reviewed"},
		{"required review", true, false, "available", "Human Review", "needs_evidence"},
		{"required checks", false, true, "available", "Human Review", "needs_evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newChangeFixture(t, nil)
			states := append(nativeFixtureStates(), tracker.NativeState{Name: "Human Review", Transitions: []string{"Merging"}}, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}})
			raw, err := json.Marshal(states)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json=? WHERE id=?", raw, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, state := range states[3:] {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)", f.project.ID, state.Name, state.Name, state.Terminal, state.Dispatchable, testTimestamp, testTimestamp); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Human Review') WHERE native_id=?", f.project.ID, f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			rules := f.rules
			rules.RequireReview = test.review
			if !test.checks {
				rules.RequiredChecks = []tracker.ChangeCheckSpec{}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "publication-rules"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			executor := hubOperatorExecutor{f.service}
			created, err := executor.Execute(ctx, changeToolCall(operatortool.CreateChange, operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), RequestID: "operator-create", Title: "Human source"}))
			if err != nil {
				t.Fatal(err)
			}
			var result operatortool.ChangeResult
			if err := json.Unmarshal(created.Content, &result); err != nil {
				t.Fatal(err)
			}
			f.change.ID, f.path = result.ChangeID, result.URL
			input := changeTestInput()
			input.Code.Availability = test.availability
			expected := ""
			args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: result.ChangeID, RequestID: "operator-publish", ExpectedVersionID: &expected, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, MergeBaseSHA: input.MergeBaseSHA, Repository: input.Repository, Code: &input.Code, Artifacts: input.Artifacts, PolicyID: input.PolicyID, External: &tracker.ChangeExternalReference{Provider: "github", ID: "42", URL: "https://github.com/example/repo/pull/42"}}
			for _, denied := range []struct {
				name string
				edit func(*operatortool.ChangeArguments)
			}{
				{"stale current version", func(a *operatortool.ChangeArguments) { v := "version_stale"; a.ExpectedVersionID = &v }},
				{"stale policy", func(a *operatortool.ChangeArguments) { a.PolicyID = "policy_stale" }},
				{"foreign project", func(a *operatortool.ChangeArguments) { a.ProjectID = "prj_foreign" }},
				{"foreign change", func(a *operatortool.ChangeArguments) { a.ChangeID = "change_foreign" }},
				{"foreign item", func(a *operatortool.ChangeArguments) { a.ItemID = "wi_foreign" }},
				{"forged PR identity", func(a *operatortool.ChangeArguments) {
					external := *a.External
					external.ID = "43"
					a.External = &external
				}},
			} {
				t.Run(denied.name, func(t *testing.T) {
					a := args
					a.RequestID = denied.name
					denied.edit(&a)
					if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, a)); err == nil {
						t.Fatal("invalid publication succeeded")
					}
				})
			}
			foreign := changeOperatorContext(t, f.service, f.token, "org_foreign")
			if _, err := executor.Execute(foreign, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("foreign organization: %v", err)
			}
			worker := changeOperatorContext(t, f.service, f.worker(t, "publication-worker"), string(f.project.OrganizationID))
			if _, err := executor.Execute(worker, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("worker operator publication: %v", err)
			}
			if len(f.detail(t).Versions) != 0 {
				t.Fatal("denied publication had effects")
			}
			call := changeToolCall(operatortool.PublishChangeVersion, args)
			published, err := executor.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(published.Content, &result); err != nil {
				t.Fatal(err)
			}
			if result.Version == nil || result.Detail == nil || result.Detail.Change.CurrentVersion != result.Version.ID || result.WorkItemState != test.lane || result.Detail.Summary.Status != test.status || result.Detail.Summary.ExternalReview != "external_gate" {
				t.Fatalf("publication effects: %s", published.Content)
			}
			version := *result.Version
			if version.RunID != "" || version.AttemptID != "" || version.Actor != (tracker.Actor{Kind: "human", PrincipalID: operatortool.ConnectionIdentity(ctx).PrincipalID}) || version.Code.Availability != test.availability || version.External == nil || *version.External != *args.External || len(version.Checks) != len(rules.RequiredChecks) {
				t.Fatalf("forged publication identity or policy: %#v", version)
			}
			originalReceipt := result.Receipt
			connection := operatortool.CurrentConnection(ctx)
			connection.ID = "publication-reconnect"
			ctx = operatortool.WithConnection(ctx, connection)
			replayed, err := executor.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(replayed.Content, &result) != nil || !bytes.Equal(originalReceipt, result.Receipt) || len(f.detail(t).Versions) != 1 {
				t.Fatal("replay changed identity or repeated effects")
			}
			input.External = args.External
			rest := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: args.RequestID}, ChangeVersionInput: input})
			requireNativeStatus(t, rest, http.StatusOK)
			if !bytes.Equal(bytes.TrimSpace(rest.Body.Bytes()), originalReceipt) {
				t.Fatal("REST and MCP do not share the publication receipt")
			}
			args.HeadSHA = strings.Repeat("d", 40)
			if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("changed retry: %v", err)
			}
			args.RequestID = "stale-current"
			if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, args)); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("stale current: %v", err)
			}
			args.RequestID, expected = "second-publication", version.ID
			if _, err := executor.Execute(ctx, changeToolCall(operatortool.PublishChangeVersion, args)); err != nil {
				t.Fatal(err)
			}
			replayed, err = executor.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			if json.Unmarshal(replayed.Content, &result) != nil || result.Version.ID != version.ID || result.Detail.Change.CurrentVersion == version.ID || !bytes.Equal(originalReceipt, result.Receipt) || len(f.detail(t).Versions) != 2 {
				t.Fatal("historical replay lost original receipt or live current version")
			}

			if test.review {
				reviewArgs := operatortool.ChangeArguments{ProjectID: args.ProjectID, ItemID: args.ItemID, ChangeID: args.ChangeID, VersionID: result.Detail.Change.CurrentVersion, ExpectedRevision: int64(result.Detail.Change.Revision), Decision: "approved", RequestID: "source-review"}
				artifacts, err := executor.Execute(ctx, changeToolCall(operatortool.ArtifactReferences, operatortool.ChangeArguments{ProjectID: args.ProjectID, ItemID: args.ItemID}))
				var references operatortool.ChangeResult
				if err != nil || json.Unmarshal(artifacts.Content, &references) != nil || len(references.Artifacts) != 0 {
					t.Fatalf("source-only artifact references: %s %v", artifacts.Content, err)
				}
				reviewCall := changeToolCall(operatortool.ReviewChange, reviewArgs)
				if _, err := executor.Execute(worker, reviewCall); !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("worker source-only review: %v", err)
				}
				reviewed, err := executor.Execute(ctx, reviewCall)
				if err != nil {
					t.Fatalf("bundle-free operator review: %v", err)
				}
				var reviewResult operatortool.ChangeResult
				if json.Unmarshal(reviewed.Content, &reviewResult) != nil {
					t.Fatalf("review result: %s", reviewed.Content)
				}
				replayedReview, err := executor.Execute(ctx, reviewCall)
				var replayResult operatortool.ChangeResult
				if err != nil || json.Unmarshal(replayedReview.Content, &replayResult) != nil || !bytes.Equal(reviewResult.Receipt, replayResult.Receipt) {
					t.Fatalf("review replay: %s %v", replayedReview.Content, err)
				}
				read, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, operatortool.ChangeArguments{ProjectID: args.ProjectID, ItemID: args.ItemID, ChangeID: args.ChangeID, VersionID: reviewArgs.VersionID, Section: "reviews"}))
				var readResult operatortool.ChangeResult
				if err != nil || json.Unmarshal(read.Content, &readResult) != nil || readResult.Detail == nil || len(readResult.Detail.Reviews) != 1 {
					t.Fatalf("exact-version review read: %s %v", read.Content, err)
				}
				review := readResult.Detail.Reviews[0]
				if review.VersionID != reviewArgs.VersionID || review.Decision != "approved" || review.Actor != version.Actor || review.Validator != nil || review.ImplementationReview != nil || review.SecurityAudit != nil {
					t.Fatalf("untruthful operator review evidence: %+v", review)
				}
				var receipt tracker.ChangeReview
				if json.Unmarshal(reviewResult.Receipt, &receipt) != nil || receipt.ID == "" || receipt.ID != review.ID {
					t.Fatalf("review receipt differs from stored evidence: %s", reviewResult.Receipt)
				}
			}

			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id=(SELECT id FROM api_tokens WHERE token_hash=?)", operatortool.ConnectionIdentity(ctx).CredentialID); err != nil {
				t.Fatal(err)
			}
			if _, err := executor.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("lost authority replay: %v", err)
			}
		})
	}
}

func TestOperatorMutationRefusalPrecedence(t *testing.T) {
	limit := &hostedLimitError{Resource: "collaboration_bytes", Allowance: 10, Consumption: 11}
	for _, projection := range []struct {
		name    string
		project func(error) error
	}{
		{"work", safeNativeWorkError},
		{"Change", hubSafeChangeError},
		{"project", projectToolError},
		{"workspace", safeWorkspaceError},
	} {
		for _, test := range []struct {
			name      string
			err, want error
		}{
			{"uncertain outcome", errors.Join(mutation.ErrUncertain, limit), mutation.ErrUncertain},
			{"access denied", errors.Join(operatortool.ErrAccessDenied, limit), operatortool.ErrAccessDenied},
			{"native access denied", errors.Join(&nativeError{Code: "access_denied", status: http.StatusForbidden}, limit), operatortool.ErrAccessDenied},
			{"denied revision", errors.Join(&nativeError{Code: "revision_conflict", status: http.StatusForbidden}, limit), operatortool.ErrAccessDenied},
		} {
			t.Run(projection.name+"/"+test.name, func(t *testing.T) {
				projected := projection.project(test.err)
				var allowance *operatortool.AllowanceError
				if !errors.Is(projected, test.want) || errors.As(projected, &allowance) {
					t.Fatalf("projected refusal=%v want=%v without quota details", projected, test.want)
				}
			})
		}
	}
}

// Catches discovery bypass reaching foreign nested resources, stale review
// identities, and replay repeating discussion effects after reconnect.
func TestOperatorChangeCommands(t *testing.T) {
	f := newChangeFixture(t, openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}))
	version := f.publish(t, "v1", "")
	bundle := seedReviewBundle(t, f, version, time.Now().Add(time.Hour))
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	executor := hubOperatorExecutor{f.service}
	base := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID}
	for _, test := range []struct {
		name, tool string
		edit       func(*operatortool.ChangeArguments)
		want       error
	}{
		{"detail", operatortool.GetChange, nil, nil},
		{"PR panel", operatortool.ListWorkItemPullRequests, func(a *operatortool.ChangeArguments) { a.ChangeID = "" }, nil},
		{"missing library", operatortool.ArtifactLibrary, func(a *operatortool.ChangeArguments) { a.ChangeID = ""; a.ItemID = "" }, operatortool.ErrServiceUnavailable},
		{"version", operatortool.GetChangeVersion, func(a *operatortool.ChangeArguments) { a.VersionID = version.ID }, nil},
		{"foreign project", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.ProjectID = "prj_foreign" }, operatortool.ErrAccessDenied},
		{"foreign item", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.ItemID = "wi_foreign" }, operatortool.ErrAccessDenied},
		{"foreign change", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.ChangeID = "change_foreign" }, operatortool.ErrAccessDenied},
		{"foreign version", operatortool.GetChangeVersion, func(a *operatortool.ChangeArguments) { a.VersionID = "version_foreign" }, operatortool.ErrAccessDenied},
		{"foreign selected version", operatortool.GetChange, func(a *operatortool.ChangeArguments) { a.VersionID = "version_foreign"; a.Section = "discussion" }, operatortool.ErrAccessDenied},
		{"missing publication identity", operatortool.PublishChangeVersion, nil, operatortool.ErrInvalidArguments},
		{"worker landing", "land_change", nil, operatortool.ErrUnknownTool},
		{"worker CI", "submit_change_check", nil, operatortool.ErrUnknownTool},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := base
			if test.edit != nil {
				test.edit(&args)
			}
			result, err := executor.Execute(ctx, changeToolCall(test.tool, args))
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}

			if test.tool == operatortool.ListWorkItemPullRequests {
				var value operatortool.ChangeResult
				if json.Unmarshal(result.Content, &value) != nil || len(value.PullRequests) != 1 || value.PullRequests[0].ChangeID != f.change.ID || value.PullRequests[0].FetchedAt.IsZero() {
					t.Fatalf("missing PR panel identity/freshness: %s", result.Content)
				}
			}
		})
	}

	at := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	environment := gate.CheckEnvironment{OS: "linux", Architecture: "amd64", GoVersion: "go1.26.6"}
	checked := gate.CheckObservation{Scope: "lint", Command: "make lint", HeadSHA: version.HeadSHA, TreeSHA: strings.Repeat("d", 40), Environment: environment, StartedAt: at.Add(-time.Minute), FinishedAt: at.Add(-time.Second)}
	scheduledCheck := checked
	scheduledCheck.HeadSHA = strings.Repeat("e", 40)
	scheduledCheck.StartedAt, scheduledCheck.FinishedAt, scheduledCheck.ExitCode = at.Add(time.Minute), at.Add(2*time.Minute), 1
	scheduled := gate.ScheduledEvidence{Schema: 1, Repository: "example/repo", OccurrenceKey: strings.Repeat("f", 64), RunID: "123", RunAttempt: "2", JobID: "456", JobName: "Lint", RunURL: "https://github.com/example/repo/actions/runs/123/attempts/2", JobURL: "https://github.com/example/repo/actions/jobs/456", HeadSHA: scheduledCheck.HeadSHA, Conclusion: "failure", Checks: []gate.CheckObservation{scheduledCheck}}
	report := f.create(t, "scheduled failure")
	stamp := issueorigin.Stamp("Scheduled job failed"+scheduled.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: strings.TrimSuffix(scheduled.RunURL, "/attempts/2"), Fingerprint: strings.Repeat("a", 64)})
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", stamp, report.WorkItemID); err != nil {
		t.Fatal(err)
	}
	report.Body = stamp
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	registered := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	registered.enroll(t)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nativeRunID := newNativeID("run")
	nativeAttempt := seedHealthRateAttempt(t, tx, scope, f.issue, string(registered.binding.MachineID), "succeeded", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Phase: "completed", HeartbeatAt: at}}, at, -1)
	if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET run_id=?,data_json=json_set(data_json,'$.attempt_id',id,'$.run_id',?) WHERE id=?", nativeRunID, nativeRunID, nativeAttempt); err != nil {
		t.Fatal(err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, want  string
		diagnostics []string
		capture     bool
		edit        func(*tracker.NativeLandingReceipt)
	}{
		{name: "missing local evidence", want: "missing_local_evidence", edit: func(r *tracker.NativeLandingReceipt) { r.Gate = nil }},
		{name: "nonzero local result", want: "local_nonzero", edit: func(r *tracker.NativeLandingReceipt) { r.Gate.ExitCode = 7 }},
		{name: "long public timeout receipt", want: "local_nonzero", diagnostics: []string{"panic: test timed out after 1m0s", "TestGateReceipt/世界", "internal/worker.TestGateReceipt", "FAIL\tgithub.com/example-org/example-project/internal/worker\t61.366s", "truncated"}, edit: func(r *tracker.NativeLandingReceipt) {
			r.Gate.ReceiptID = "receipt_timeout"
			r.Gate.ExitCode = 2
			r.Gate.Output = strings.Repeat("ok example.org/other/package\n", 300) +
				"panic: test timed out after 1m0s\nrunning tests:\n\tTestGateReceipt/世界 (1m0s)\n\n" +
				"github.com/example-org/example-project/internal/worker.TestGateReceipt(0xc000001)\n\t/Users/private/project/worker_test.go:42\n" +
				"Authorization: Bearer private-bearer\nDEPLOY_TOKEN=private-token\nhttps://private.example/log?secret=private-url\n" +
				strings.Repeat("goroutine 99 [select]:\n\t/home/private/project/worker.go:12 世界\n", 300) +
				"FAIL\tgithub.com/example-org/example-project/internal/worker\t61.366s\n"
			public := tracker.PublicGateResult(*r.Gate)
			r.Gate = &public
		}},
		{name: "captured migration failure before frontend success", want: "local_nonzero", capture: true,
			diagnostics: []string{"./scripts/check-migration-order.sh dev", "MIGRATION ORDERING CONFLICT DETECTED!", "20261017090000_example.sql", "stage=repository exit_status=1", "[redacted]", "truncated"},
			edit: func(r *tracker.NativeLandingReceipt) {
				r.Gate.ExitCode = 2
				r.Gate.Output = strings.Repeat("ok repository setup\n", 300) +
					"./scripts/check-migration-order.sh dev\nMIGRATION ORDERING CONFLICT DETECTED!\nmigrations/20261017090000_example.sql is below dev migration 20261017100000_other.sql\ncheck-fast-stage stage=repository exit_status=1\n" +
					"Authorization: Bearer private-bearer\nTOKEN=private-assignment\n/home/private/project/build.log\n" +
					strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\n"
			}},
		{name: "captured terminal make failure preserves preceding diagnostic", want: "local_nonzero", capture: true,
			diagnostics: []string{"./scripts/check-repository.sh", "Repository schema mismatch 世界", "migrations/example.sql: missing delivery column", "expected column: delivery_id", "actual column: legacy_id", "make: *** [check-repository] Error 2", "[redacted]", "truncated"},
			edit: func(r *tracker.NativeLandingReceipt) {
				r.Gate.ExitCode = 2
				r.Gate.Output = strings.Repeat(" PASS frontend suite 世界\n", 8000) +
					"./scripts/check-repository.sh\nRepository schema mismatch 世界\nmigrations/example.sql: missing delivery column\nexpected column: delivery_id\nactual column: legacy_id\nTOKEN=private-assignment\n/home/private/project/build.log\nmake: *** [check-repository] Error 2\n"
			}},
		{name: "different checked content", want: "different_content", edit: func(r *tracker.NativeLandingReceipt) { r.Gate.TreeSHA = strings.Repeat("a", 40) }},
		{name: "scope differs", want: "different_check_scope", edit: func(r *tracker.NativeLandingReceipt) {
			r.Gate.Evidence.Checks[0].Scope = "unit-short"
			r.Gate.Evidence.Checks[0].Command = "make test-fast"
		}},
		{name: "unrecorded check scope", want: "unknown", edit: func(r *tracker.NativeLandingReceipt) { r.Gate.Evidence.Checks = nil }},
		{name: "environment differs", want: "different_environment", edit: func(r *tracker.NativeLandingReceipt) { r.Gate.Evidence.Checks[0].Environment.OS = "darwin" }},
		{name: "unknown historical metadata", want: "unknown", edit: func(r *tracker.NativeLandingReceipt) { r.Gate.Evidence = nil }},
		{name: "same content and check passes locally", want: "local_pass_scheduled_failure", edit: func(r *tracker.NativeLandingReceipt) { r.Gate.Output = strings.Repeat("界", 21000) }},
		{name: "earlier Change is only a candidate", want: "local_pass_scheduled_failure", edit: func(r *tracker.NativeLandingReceipt) { r.MergeSHA = strings.Repeat("9", 40) }},
	} {
		t.Run("validation audit "+test.name, func(t *testing.T) {
			receipt := tracker.NativeLandingReceipt{ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: scheduled.HeadSHA, Landed: true, ObservedAt: at, Gate: &gate.CommandResult{Command: "make check-land", HeadSHA: checked.HeadSHA, TreeSHA: checked.TreeSHA, Output: "private prompt and credentials", Evidence: &gate.CommandEvidence{Environment: environment, Checks: []gate.CheckObservation{checked}}}}
			if test.edit != nil {
				test.edit(&receipt)
			}
			if test.capture {
				if testing.Short() || runtime.GOOS == "windows" {
					t.Skip("git and shell subprocess integration")
				}
				source := t.TempDir()
				git := func(args ...string) string {
					command := exec.CommandContext(ctx, "git", append([]string{"-c", "init.defaultBranch=main", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
					command.Dir = source
					output, err := command.CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v\n%s", args, err, output)
					}
					return strings.TrimSpace(string(output))
				}
				git("init", "-q", ".")
				if err := os.WriteFile(filepath.Join(source, "output.txt"), []byte(receipt.Gate.Output), 0o600); err != nil {
					t.Fatal(err)
				}
				git("add", "output.txt")
				git("commit", "-qm", "gate output fixture")
				backend, err := workspace.NewLocalGitContext(ctx, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
				if err != nil {
					t.Fatal(err)
				}
				issue := workspace.Issue{Identifier: "gate-output"}
				info, err := backend.Create(ctx, issue)
				if err != nil {
					t.Fatal(err)
				}
				issue.PullRequestHeadSHA = git("rev-parse", "HEAD")
				captured, err := backend.RunReviewCommand(ctx, info, issue, "cat output.txt; exit 2")
				if err != nil || captured.ExitCode != 2 || !captured.OutputTruncated || len(captured.Output) > 64*1024 || captured.HeadSHA != issue.PullRequestHeadSHA || captured.TreeSHA == "" {
					t.Fatalf("command capture lost failure status, bounds or source identity: %+v %v", captured, err)
				}
				receipt.Gate.Output = captured.Output
				receipt.Gate.OutputTruncated = captured.OutputTruncated
				public := tracker.PublicGateResult(*receipt.Gate)
				receipt.Gate = &public
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := persistAttemptLanding(t.Context(), tx, nativeAttempt, receipt); err != nil {
				t.Fatal(err)
			}
			if len(test.diagnostics) > 0 {
				raw, err := json.Marshal(receipt.Gate)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json, '$.runtime.validation', json(?)) WHERE id=?", raw, nativeAttempt); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, base))
			var value operatortool.ChangeResult
			if err != nil || json.Unmarshal(result.Content, &value) != nil || value.ValidationAudit != nil {
				t.Fatalf("Change read repeated project audit: %v", err)
			}
			if test.name == "same content and check passes locally" {
				want := *receipt.Gate
				want.Output, want.OutputTruncated = "[earlier output truncated]\n"+strings.Repeat("界", 1356), true
				if len(value.Detail.Versions) != 1 || value.Detail.Versions[0].Landing == nil || !reflect.DeepEqual(value.Detail.Versions[0].Landing.Gate, &want) {
					t.Fatal("landing receipt lost identity or explicit output excerpt")
				}
				stored, err := readVersionLanding(ctx, f.service.database.db, version.ID)
				if err != nil || !reflect.DeepEqual(stored.Gate, receipt.Gate) {
					t.Fatalf("bounded landing read changed canonical output: %v", err)
				}
			}
			if len(test.diagnostics) > 0 {
				if len(value.Detail.Versions) != 1 || value.Detail.Versions[0].Landing == nil {
					t.Fatal("missing public landing receipt")
				}
				got := value.Detail.Versions[0].Landing.Gate
				if !reflect.DeepEqual(got, receipt.Gate) || !got.OutputTruncated || len(got.Output) > 4096 || !utf8.ValidString(got.Output) || strings.Contains(got.Output, "private") {
					t.Fatal("Change read lost bounded public failure evidence or receipt identity")
				}
				for _, diagnostic := range test.diagnostics {
					if !strings.Contains(got.Output, diagnostic) {
						t.Fatalf("Change failure excerpt missing %q", diagnostic)
					}
				}
			}
			projectAudit, err := readValidationAudit(ctx, f.service.database.db, scope, "", "", f.change.ID, "")
			if err != nil || len(projectAudit.Occurrences) != 1 {
				t.Fatalf("audit=%+v %v", projectAudit, err)
			}
			occurrence := projectAudit.Occurrences[0]
			if occurrence.Source.WorkItemID != report.WorkItemID || occurrence.Source.Actor.PrincipalID == "" || len(occurrence.Comparisons) != 1 {
				t.Fatalf("source/candidates=%+v", occurrence)
			}
			comparison := occurrence.Comparisons[0]
			if comparison.Outcome != test.want || comparison.NativeAttemptID != nativeAttempt || comparison.NativeRunID != nativeRunID || comparison.LocalAttemptID != 168 || comparison.VersionID != version.ID || comparison.ChangeID != f.change.ID || comparison.Local != nil && comparison.Local.Output != "" {
				t.Fatalf("comparison=%+v want=%s", comparison, test.want)
			}
			if test.name == "earlier Change is only a candidate" && (comparison.IntegratedSource != "different_commit" || !strings.Contains(comparison.Attribution, "unverified")) {
				t.Fatalf("invented attribution=%+v", comparison)
			}
			for _, read := range []struct{ name, reference, attempt string }{
				{operatortool.WorkItem, string(report.WorkItemID), ""},
				{operatortool.WorkAttemptReceipt, string(f.issue.WorkItemID), nativeAttempt},
			} {
				args := map[string]string{"project_id": string(f.project.ID), "reference": read.reference}
				if read.attempt != "" {
					args["native_attempt_id"] = read.attempt
				}
				raw, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				result, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: read.name, Arguments: raw})
				var envelope struct {
					Data struct {
						Audit   *tracker.ValidationAudit `json:"validation_audit"`
						Attempt *tracker.NativeAttempt   `json:"attempt"`
					} `json:"data"`
				}
				if err != nil || json.Unmarshal(result.Content, &envelope) != nil || envelope.Data.Audit == nil || len(envelope.Data.Audit.Occurrences) != 1 || envelope.Data.Audit.Occurrences[0].Comparisons[0].Outcome != test.want {
					t.Fatalf("%s projection=%s %v", read.name, result.Content, err)
				}
				if len(test.diagnostics) > 0 && read.name == operatortool.WorkAttemptReceipt {
					attempt := envelope.Data.Attempt
					if attempt == nil || attempt.AttemptID != nativeAttempt || attempt.Runtime == nil || attempt.Runtime.Landing == nil || !reflect.DeepEqual(attempt.Runtime.Landing.Gate, receipt.Gate) || !reflect.DeepEqual(attempt.Runtime.Validation, receipt.Gate) {
						t.Fatal("MCP attempt projection lost original public failure evidence or identity")
					}
				}
			}
			audit, err := readValidationAudit(t.Context(), f.service.database.db, scope, string(report.WorkItemID), "", "", "")
			if err != nil || len(audit.Occurrences) != 1 || audit.Occurrences[0].Comparisons[0].Outcome != test.want {
				t.Fatalf("scheduled read=%+v %v", audit, err)
			}
		})
	}
	t.Run("validation audit bounded observations", func(t *testing.T) {
		large := scheduled
		large.OccurrenceKey = strings.Repeat("1", 64)
		large.Checks = nil
		for range 32 {
			large.Checks = append(large.Checks, scheduledCheck)
		}
		body := issueorigin.Stamp("Scheduled job failed"+large.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: large.RunURL, Fingerprint: strings.Repeat("a", 64)})
		duplicate := large
		duplicate.OccurrenceKey = strings.Repeat("2", 64)
		body += duplicate.Stamp()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(report.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "bounded-observations"}, Body: body})
		requireNativeStatus(t, response, http.StatusOK)
		var comment tracker.NativeComment
		decodeHubResponse(t, response, &comment)
		audit, err := readValidationAudit(t.Context(), f.service.database.db, scope, string(report.WorkItemID), "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(audit)
		if err != nil || !audit.Partial || len(raw) > 64*1024 || len(audit.Occurrences) == 0 || audit.Occurrences[0].Source.RecordID != comment.ID {
			t.Fatalf("unbounded audit: bytes=%d partial=%v occurrences=%d %v", len(raw), audit.Partial, len(audit.Occurrences), err)
		}
	})
	foreignScope := nativeScope{organization: scope.organization, project: "prj_foreign"}
	foreignAudit, err := readValidationAudit(t.Context(), f.service.database.db, foreignScope, "", "", "", "")
	if err != nil || len(foreignAudit.Occurrences) != 0 {
		t.Fatalf("foreign evidence=%+v %v", foreignAudit, err)
	}
	args := base
	args.RequestID = "discussion"
	args.Body = "message"
	call := changeToolCall(operatortool.DiscussChange, args)
	first, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	reconnect := operatortool.CurrentConnection(ctx)
	reconnect.ID = "reconnected"
	ctx = operatortool.WithConnection(ctx, reconnect)
	second, err := executor.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var one, two operatortool.ChangeResult
	if json.Unmarshal(first.Content, &one) != nil || json.Unmarshal(second.Content, &two) != nil || !bytes.Equal(one.Receipt, two.Receipt) {
		t.Fatal("replay changed command receipt")
	}
	args.Body = "different"
	if _, err := executor.Execute(ctx, changeToolCall(operatortool.DiscussChange, args)); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if count := len(f.detail(t).Discussion); count != 1 {
		t.Fatalf("discussion effects=%d", count)
	}
	args = base
	args.VersionID = version.ID
	args.RequestID = "review"
	args.ExpectedRevision = int64(f.detail(t).Change.Revision)
	args.Decision = "approved"
	args.Bundle = &bundle
	for _, test := range []struct {
		name string
		edit func(*operatortool.ChangeArguments)
	}{
		{"stale revision", func(a *operatortool.ChangeArguments) { a.ExpectedRevision++ }},
		{"forged bundle", func(a *operatortool.ChangeArguments) {
			b := *a.Bundle
			b.SHA256 = strings.Repeat("f", 64)
			a.Bundle = &b
		}},
		{"stale version", func(a *operatortool.ChangeArguments) { a.VersionID = "version_stale" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := args
			test.edit(&a)
			authorized, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: "write", ProjectID: a.ProjectID})
			if err != nil {
				t.Fatal(err)
			}
			app, err := operatortool.CurrentChanges(authorized)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = app.MutateChange(authorized, operatortool.ReviewChange, a); err == nil {
				t.Fatal("invalid review succeeded")
			}
		})
	}
	if len(f.detail(t).Reviews) != 0 {
		t.Fatal("denied review had side effects")
	}
	if _, err := executor.Execute(ctx, changeToolCall(operatortool.ReviewChange, args)); err != nil {
		t.Fatalf("direct review: %v", err)
	}

	// Catches creation returning only the caller's empty selector rather than
	// the new application identity needed for a subsequent detail call.
	linked := f.create(t, "linked operator work")
	createArgs := operatortool.ChangeArguments{ProjectID: base.ProjectID, ItemID: base.ItemID, RequestID: "new-change", Title: "Follow-up change", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}}
	created, err := executor.Execute(ctx, changeToolCall(operatortool.CreateChange, createArgs))
	if err != nil {
		t.Fatal(err)
	}
	var createdResult operatortool.ChangeResult
	if json.Unmarshal(created.Content, &createdResult) != nil || createdResult.ChangeID == "" || !strings.HasSuffix(createdResult.URL, "/changes/"+createdResult.ChangeID) {
		t.Fatalf("unusable creation result: %s", created.Content)
	}
	changes, err := f.service.readChanges(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(linked.WorkItemID))
	if err != nil || len(changes) != 1 || changes[0].ID != createdResult.ChangeID {
		t.Fatalf("lost linked Change: %+v %v", changes, err)
	}
	replayed, err := executor.Execute(ctx, changeToolCall(operatortool.CreateChange, createArgs))
	var replayResult operatortool.ChangeResult
	if err != nil || json.Unmarshal(replayed.Content, &replayResult) != nil || replayResult.ChangeID != createdResult.ChangeID {
		t.Fatalf("linked Change replay=%s %v", replayed.Content, err)
	}
	other := newNativeFixture(t, f.service, "", "foreign-linked-project")
	foreign := other.create(t, "foreign")
	createArgs.RequestID = "foreign-link"
	createArgs.LinkedIssues = []tracker.NativeWorkItemID{foreign.WorkItemID}
	if _, err := executor.Execute(ctx, changeToolCall(operatortool.CreateChange, createArgs)); err == nil {
		t.Fatal("cross-project Change linkage succeeded")
	}
	t.Run("hosted get_change failure logs once", func(t *testing.T) {
		var snapshot, trigger string
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT record_json FROM change_versions WHERE id=?", version.ID).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT sql FROM sqlite_master WHERE type='trigger' AND name='change_versions_immutable'").Scan(&trigger); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER change_versions_immutable"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_versions SET record_json=? WHERE id=?", `["tenant content sentinel"]`, version.ID); err != nil {
			t.Fatal(err)
		}
		cleanupCtx := context.WithoutCancel(t.Context())
		t.Cleanup(func() {
			if _, err := f.service.database.db.ExecContext(cleanupCtx, "UPDATE change_versions SET record_json=? WHERE id=?", snapshot, version.ID); err != nil {
				t.Error(err)
			}
			if _, err := f.service.database.db.ExecContext(cleanupCtx, trigger); err != nil {
				t.Error(err)
			}
		})
		var logs hostedLogBuffer
		f.service.config.Logger = slog.New(hostedLogHandler{output: logging.NewHandler(&logs, slog.LevelInfo, false, logging.SourceSetting{})})
		for _, transport := range []string{"stdio", "http"} {
			t.Run(transport, func(t *testing.T) {
				before := logs.String()
				call := hostedContextProtocol(t, f.service, ctx, transport)
				reply := call("tools/call", operatortool.GetChange, map[string]any{"project_id": base.ProjectID, "work_item_id": base.ItemID, "change_id": base.ChangeID})
				logged := strings.TrimPrefix(logs.String(), before)
				if strings.Count(logged, "\n") != 1 || strings.Count(logged, `"error":`) != 1 {
					t.Fatalf("expected one fault log: %s", logged)
				}
				var record map[string]any
				if err := json.Unmarshal([]byte(logged), &record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] != "operator tool failed" || record["tool"] != operatortool.GetChange || record["error_class"] != "*json.UnmarshalTypeError" || record["organization_id"] != string(f.project.OrganizationID) {
					t.Fatal(logged)
				}
				trail, _ := record["error"].(string)
				if !strings.Contains(trail, "get change:") || !strings.Contains(trail, "load detail: decode change version [redacted]: json: cannot unmarshal array") {
					t.Fatal(logged)
				}
				source, ok := record["source"].(map[string]any)
				if !ok || !strings.HasSuffix(source["file"].(string), "internal/mcp/server.go") || source["line"].(float64) <= 0 {
					t.Fatal(logged)
				}
				var result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				}
				if err := json.Unmarshal(reply.Result, &result); err != nil {
					t.Fatal(err)
				}
				want := "Operator tool is unavailable (reason_code service_unavailable, correlation_id " + record["correlation_id"].(string) + ")"
				if !result.IsError || len(result.Content) != 1 || result.Content[0].Text != want || strings.Contains(logged+string(reply.Result), "tenant content sentinel") {
					t.Fatalf("log=%s reply=%s", logged, reply.Result)
				}
			})
		}
	})
	t.Run("bounded current evidence and immutable history", func(t *testing.T) {
		ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
		workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{Workflow: []byte(strings.Repeat("résumé 世界\n", 2800)), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/orders\n  lanes:\n    - {name: Todo, role: active}\n    - {name: In Progress, role: active}\n    - {name: Done, role: terminal}\n    - {name: Blocked, role: holding}\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress, Done]\n      In Progress: [Todo, Done]\n      Done: [Todo]\n"), HasConfig: true, ConfigPath: "detent.yaml"})
		if err != nil {
			t.Fatal(err)
		}
		workflow.Definition.Revision = strings.Repeat("a", 40)
		descriptor, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{Policy: descriptor, ExpectedID: hubTestPolicy().ID}), http.StatusOK)
		rules := f.rules
		rules.PolicyID = descriptor.ID
		response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "large-policy-rules"}, ExpectedID: rules.ID, Policy: rules})
		requireNativeStatus(t, response, http.StatusOK)
		var current = version
		versions := map[string]tracker.ChangeVersion{version.ID: version}
		for i := range 6 {
			input := changeTestInput()
			input.PolicyID = descriptor.ID
			input.HeadSHA = strings.Repeat(strconv.Itoa(i+1), 40)
			bundle := changeTestSource(t, &input)
			input.Validation = &gate.CommandResult{Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Output: strings.Repeat("界", 21000), Evidence: &gate.CommandEvidence{}}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "large-version-" + strconv.Itoa(i)}, ExpectedVersionID: current.ID, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &current)
			versions[current.ID] = current
		}
		policyBytes, err := json.Marshal(current.Policy)
		if err != nil || len(policyBytes) < 48000 || len(policyBytes) > 60000 {
			t.Fatalf("unrealistic policy bytes=%d: %v", len(policyBytes), err)
		}
		for i := range 3 {
			unrelated := f.create(t, "unrelated scheduled failure")
			record := scheduled
			record.OccurrenceKey = policy.Digest([]byte(strconv.Itoa(i)))
			record.RunID = strconv.Itoa(200 + i)
			record.RunURL = "https://github.com/example-org/example-project/actions/runs/" + record.RunID
			body := issueorigin.Stamp("Scheduled job failed"+record.Stamp(), issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: record.RunURL, Fingerprint: record.OccurrenceKey})
			if _, err := f.service.database.db.ExecContext(ctx, "UPDATE issues SET body=? WHERE native_id=?", body, unrelated.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}
		for _, item := range []tracker.NativeIssue{f.issue, report} {
			raw, err := json.Marshal(map[string]string{"project_id": string(f.project.ID), "reference": string(item.WorkItemID)})
			if err != nil {
				t.Fatal(err)
			}
			result, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkItem, Arguments: raw})
			var envelope struct {
				Data operatortool.NativeItem `json:"data"`
			}
			if err != nil || json.Unmarshal(result.Content, &envelope) != nil || envelope.Data.WorkItemID != item.WorkItemID || envelope.Data.Body != item.Body {
				t.Fatalf("selected work item lost content: %v", err)
			}
			if item.WorkItemID == f.issue.WorkItemID {
				if envelope.Data.ValidationAudit != nil {
					t.Fatal("ordinary work item repeats project validation audit")
				}
			} else if audit := envelope.Data.ValidationAudit; audit == nil || len(audit.Occurrences) != 1 || audit.Occurrences[0].Source.WorkItemID != report.WorkItemID || !strings.Contains(audit.Occurrences[0].Comparisons[0].Attribution, "unverified") {
				t.Fatal("scheduled work item lost its own evidence or included unrelated reports")
			}
		}
		insert := func(ctx context.Context, kind, versionID string, record any) {
			t.Helper()
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(ctx, "INSERT INTO change_evidence (change_id,version_id,kind,record_json) VALUES (?,?,?,?)", f.change.ID, versionID, kind, raw); err != nil {
				t.Fatal(err)
			}
		}
		assertVersion := func(got tracker.ChangeVersion) {
			t.Helper()
			want := versions[got.ID]
			if want.Policy.Authored != nil {
				authored := *want.Policy.Authored
				authored.Files = nil
				want.Policy.Authored = &authored
			}
			want.Policy.Configuration, want.Policy.Workflow = nil, nil
			if want.Validation != nil {
				command := *want.Validation
				command.Output, command.OutputTruncated = "[earlier output truncated]\n"+strings.Repeat("界", 1356), true
				want.Validation = &command
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("version identity, gate requirements or validation receipt changed beyond disclosed policy details and output excerpt")
			}
		}
		actor := current.Actor
		for i := range 9 {
			id := strconv.Itoa(i)
			validator := &gate.ValidatorResult{VersionID: current.ID, HeadSHA: current.HeadSHA, Verdict: "approved", Submitted: true}
			if i == 0 {
				validator.Verdict = gate.ValidatorVerdictRework
				validator.NotVerified = []string{"missing journey"}
				validator.Findings = []gate.Finding{{Severity: "p1", Body: "failed journey"}}
				validator.CriteriaEvidence = []gate.CriterionEvidence{{Criterion: "missing journey", Kind: gate.EvidenceNotVerified, Reference: "test journey", ReceiptIDs: []string{"receipt_0", "receipt_1"}, HeadSHA: current.HeadSHA, TreeSHA: strings.Repeat("d", 40), Behavior: "failed command"}}
				for command := range 5 {
					output := strings.Repeat("界", 21000)
					if command == 4 {
						output = "short diagnostic <failure>\n"
					}
					validator.Commands = append(validator.Commands, gate.CommandResult{ReceiptID: "receipt_" + strconv.Itoa(command), Command: "check " + strconv.Itoa(command), HeadSHA: current.HeadSHA, TreeSHA: strings.Repeat("d", 40), ExitCode: command % 2, DurationNS: 1, Output: output, OutputTruncated: command >= 3})
				}
			}
			review := tracker.ChangeReview{ID: "review_" + id, VersionID: current.ID, Decision: "commented", Body: strings.Repeat("é<\n", 5000), Actor: actor, CreatedAt: at, Validator: validator}
			if i%2 == 0 {
				review.ImplementationReview, review.Validator = review.Validator, nil
			}
			insert(ctx, "review", current.ID, review)
			insert(ctx, "check", current.ID, tracker.ChangeCheck{VersionID: current.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: "check_" + id, HeadSHA: current.HeadSHA, Conclusion: "failure", Evidence: []tracker.ChangeArtifact{current.Code}}, Actor: actor, ReceivedAt: at})
			insert(ctx, "discussion", current.ID, tracker.ChangeDiscussion{ID: "discussion_" + id, VersionID: current.ID, Body: strings.Repeat("世界<\n", 5000), Actor: actor, CreatedAt: at})
		}
		full := f.detail(t)
		for _, stored := range full.Versions {
			versions[stored.ID] = stored
		}
		for _, review := range full.Reviews {
			if review.ID == "review_0" {
				raw, err := json.Marshal(operatortool.ChangeResult{Detail: &tracker.ChangeDetail{Change: full.Change, Versions: []tracker.ChangeVersion{current}, Reviews: []tracker.ChangeReview{review}}})
				if err != nil || len(raw) <= operatortool.MaxResultBytes {
					t.Fatalf("single output-heavy review did not exceed budget: bytes=%d %v", len(raw), err)
				}
			}
		}
		old, err := json.Marshal(operatortool.ChangeResult{Detail: &full})
		if err != nil || len(old) <= operatortool.MaxResultBytes {
			t.Fatalf("old aggregate did not reproduce the failure: bytes=%d %v", len(old), err)
		}
		read := func(args operatortool.ChangeArguments) operatortool.ChangeResult {
			t.Helper()
			result, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args))
			var value operatortool.ChangeResult
			if err != nil || len(result.Content) > operatortool.MaxResultBytes || !utf8.Valid(result.Content) || json.Unmarshal(result.Content, &value) != nil || value.ValidationAudit != nil || value.Read == nil || !value.Read.PolicyDetailsOmitted || !reflect.DeepEqual(value.Detail.Summary, full.Summary) {
				t.Fatalf("bounded read bytes=%d error=%v", len(result.Content), err)
			}
			params := url.Values{"view": {"bounded"}, "section": {args.Section}, "cursor": {args.Cursor}, "version_id": {args.VersionID}}
			if args.Limit > 0 {
				params.Set("limit", strconv.Itoa(args.Limit))
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?"+params.Encode(), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var native operatortool.ChangeResult
			decodeHubResponse(t, response, &native)
			if response.Body.Len() > operatortool.MaxResultBytes || !reflect.DeepEqual(native.Detail, value.Detail) || !reflect.DeepEqual(native.Read, value.Read) {
				t.Fatal("native and hosted projections diverged")
			}
			return value
		}
		one := base
		one.Section, one.Limit = "discussion", 1
		focused := read(one)
		focusedBytes, err := json.Marshal(focused)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("discussion_limit_1_bytes=%d", len(focusedBytes))
		if focused.ValidationAudit != nil || focused.Detail.Versions[0].Policy.Configuration != nil || focused.Detail.Versions[0].Policy.Workflow != nil || focused.Detail.Versions[0].Policy.Authored != nil && len(focused.Detail.Versions[0].Policy.Authored.Files) != 0 {
			t.Fatal("selected discussion repeats policy details or unrelated scheduled comparisons")
		}
		if len(focusedBytes) > 16<<10 || len(focused.Detail.Discussion) != 1 || !reflect.DeepEqual(focused.Detail.Discussion[0], full.Discussion[0]) || focused.Read.Sections["discussion"].Complete || focused.Read.Sections["discussion"].NextCursor == "" {
			t.Fatal("focused discussion lost evidence or continuation")
		}
		one.Cursor = focused.Read.Sections["discussion"].NextCursor
		continued := read(one)
		if len(continued.Detail.Discussion) != 1 || !reflect.DeepEqual(continued.Detail.Discussion[0], full.Discussion[1]) {
			t.Fatal("focused discussion continuation lost evidence")
		}
		one.Cursor = ""
		one.Section, one.Limit = "reviews", 1
		selected := read(one)
		if len(selected.Detail.Reviews) != 1 || selected.Detail.Reviews[0].ID != "review_0" {
			t.Fatal("output-heavy review was not readable")
		}
		first := read(base)
		currentBytes, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("old_aggregate_bytes=%d current_page_bytes=%d policy_bytes=%d versions=%d", len(old), len(currentBytes), len(policyBytes), len(versions))
		if first.Read.VersionID != current.ID || first.Read.Complete || len(first.Detail.Versions) != 1 || first.Detail.Summary.NativeReview == "approved" || first.Detail.Summary.Checks == "passed" {
			t.Fatalf("current identity or readiness lost: %+v", first.Read)
		}
		assertVersion(first.Detail.Versions[0])
		for _, section := range []string{"versions", "reviews", "checks", "discussion"} {
			seen := map[string]bool{}
			collect := func(value operatortool.ChangeResult) {
				t.Helper()
				var ids []string
				switch section {
				case "versions":
					for _, v := range value.Detail.Versions {
						assertVersion(v)
						ids = append(ids, v.ID)
					}
				case "reviews":
					for _, r := range value.Detail.Reviews {
						for _, stored := range full.Reviews {
							if stored.ID != r.ID {
								continue
							}
							want := stored
							result := stored.Validator
							if stored.ImplementationReview != nil {
								result = stored.ImplementationReview
							}
							validator := *result
							validator.Commands = append([]gate.CommandResult(nil), validator.Commands...)
							for i := range validator.Commands {
								if i < 4 {
									validator.Commands[i].Output = "[earlier output truncated]\n" + strings.Repeat("界", 1356)
									validator.Commands[i].OutputTruncated = true
								}
							}
							if stored.ImplementationReview != nil {
								want.ImplementationReview = &validator
							} else {
								want.Validator = &validator
							}
							if !reflect.DeepEqual(r, want) {
								t.Fatal("review verdict, criteria, receipt identity or output disclosure changed")
							}
						}
						ids = append(ids, r.ID)
					}
				case "checks":
					for _, c := range value.Detail.Checks {
						ids = append(ids, c.CheckRunID)
					}
				case "discussion":
					for _, d := range value.Detail.Discussion {
						ids = append(ids, d.ID)
					}
				}
				for _, id := range ids {
					if seen[id] {
						t.Fatalf("duplicated %s identity %s", section, id)
					}
					seen[id] = true
				}
			}
			collect(first)
			page := first.Read.Sections[section]
			for pages := 0; !page.Complete; pages++ {
				if pages > 20 || page.NextCursor == "" {
					t.Fatal("continuation failed to progress")
				}
				args := base
				args.Cursor, args.Limit = page.NextCursor, 2
				value := read(args)
				collect(value)
				page = value.Read.Sections[section]
			}
			want := 9
			if section == "versions" {
				want = len(versions)
			}
			if section == "discussion" {
				want = len(full.Discussion)
			}
			if len(seen) != want {
				t.Fatalf("lost %s identities: got %d want %d", section, len(seen), want)
			}
		}
		if stored := f.detail(t); !reflect.DeepEqual(stored, full) {
			t.Fatal("bounded reads changed canonical records or original command output")
		}
		fragmentReviews := map[string][]byte{}
		for _, size := range []int{operatortool.MaxResultBytes - 1, operatortool.MaxResultBytes, operatortool.MaxResultBytes + 1} {
			review := tracker.ChangeReview{ID: "review_fragment_" + strconv.Itoa(size), VersionID: current.ID, Decision: "commented", Body: "Escaped <\"\n世界"}
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			review.Body += strings.Repeat("x", size-len(raw))
			raw, err = json.Marshal(review)
			if err != nil || len(raw) != size {
				t.Fatalf("boundary fixture size=%d: %v", len(raw), err)
			}
			fragmentReviews[review.ID] = raw
			insert(ctx, "review", current.ID, review)
		}
		fragmentArgs := base
		fragmentArgs.Section, fragmentArgs.Limit = "reviews", 1
		var record []byte
		var identity tracker.NativeRecoveryPage
		fragmentCursor := ""
		for pages := 0; ; pages++ {
			if pages > 40 {
				t.Fatal("fragment pagination did not terminate")
			}
			result, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, fragmentArgs))
			var value operatortool.ChangeResult
			if err != nil || len(result.Content) > operatortool.MaxResultBytes || json.Unmarshal(result.Content, &value) != nil {
				t.Fatalf("fragment read: %v", err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?view=bounded&section=reviews&limit=1&cursor="+url.QueryEscape(fragmentArgs.Cursor), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var native operatortool.ChangeResult
			decodeHubResponse(t, response, &native)
			if response.Body.Len() > operatortool.MaxResultBytes || !reflect.DeepEqual(native.Fragment, value.Fragment) || !reflect.DeepEqual(native.Read, value.Read) {
				t.Fatal("HTTP and MCP fragments diverged")
			}
			if page := value.Fragment; page != nil {
				if len(record) == 0 {
					identity = *page
				}
				if page.OrganizationID != f.project.OrganizationID || page.ProjectID != f.project.ID || page.WorkItemID != f.issue.WorkItemID || page.VersionID != current.ID || page.Section != "reviews" || page.RecordID != identity.RecordID || page.TotalBytes != identity.TotalBytes || page.SHA256 != identity.SHA256 || page.Offset != int64(len(record)) || len(page.Data) == 0 || len(page.Data) > 64<<10 || len(value.Detail.Reviews) != 0 {
					t.Fatal("fragment lost identity or byte bounds")
				}
				record = append(record, page.Data...)
				if int64(len(record)) == page.TotalBytes {
					digest := sha256.Sum256(record)
					var review tracker.ChangeReview
					if hex.EncodeToString(digest[:]) != page.SHA256 || json.Unmarshal(record, &review) != nil || !bytes.Equal(record, fragmentReviews[review.ID]) {
						t.Fatal("fragment changed canonical bytes")
					}
					delete(fragmentReviews, review.ID)
					record = nil
				} else if fragmentCursor == "" {
					fragmentCursor = value.Read.Sections["reviews"].NextCursor
					var cursor struct {
						Offset  int64  `json:"offset,omitempty"`
						Digest  string `json:"digest,omitempty"`
						Change  string `json:"change"`
						Head    string `json:"head"`
						Version string `json:"version"`
						Section string `json:"section"`
						After   int64  `json:"after"`
						Through int64  `json:"through"`
					}
					raw, err := base64.RawURLEncoding.DecodeString(fragmentCursor)
					if err != nil || json.Unmarshal(raw, &cursor) != nil {
						t.Fatal("invalid issued fragment cursor")
					}
					for _, mode := range []string{"digest", "past end"} {
						t.Run("fragment cursor/"+mode, func(t *testing.T) {
							altered := cursor
							switch mode {
							case "digest":
								altered.Digest = strings.Repeat("0", 64)
							case "past end":
								altered.Offset = page.TotalBytes
							}
							args := fragmentArgs
							raw, err = json.Marshal(altered)
							args.Cursor = base64.RawURLEncoding.EncodeToString(raw)
							if err != nil {
								t.Fatal(err)
							}
							if _, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args)); err == nil {
								t.Fatal("altered fragment cursor accepted")
							}
						})
					}
				}
			}
			page := value.Read.Sections["reviews"]
			if page.Complete {
				break
			}
			if page.NextCursor == "" || page.NextCursor == fragmentArgs.Cursor {
				t.Fatal("fragment cursor did not advance")
			}
			fragmentArgs.Cursor = page.NextCursor
		}
		if len(record) != 0 || len(fragmentReviews) != 0 || fragmentCursor == "" {
			t.Fatal("lost boundary review evidence")
		}
		args := base
		args.VersionID = version.ID
		historical := read(args)
		if len(historical.Detail.Reviews) != 1 || historical.Detail.Reviews[0].Decision != "approved" || historical.Detail.Reviews[0].VersionID != version.ID {
			t.Fatal("lost historical approval")
		}
		args.Section = "discussion"
		args.Cursor = first.Read.Sections["reviews"].NextCursor
		if _, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args)); !errors.Is(err, operatortool.ErrInvalidArguments) {
			t.Fatalf("mixed cursor accepted: %v", err)
		}
		args = base
		args.Cursor = first.Read.Sections["versions"].NextCursor
		args.ProjectID = "prj_foreign"
		if _, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args)); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("foreign cursor accepted: %v", err)
		}
		args = base
		args.VersionID, args.ExpectedRevision, args.Decision, args.Bundle, args.RequestID = version.ID, int64(full.Change.Revision), "approved", &bundle, "old-approval-large-history"
		if _, err := executor.Execute(ctx, changeToolCall(operatortool.ReviewChange, args)); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("stale approval accepted: %v", err)
		}
		insert(ctx, "discussion", current.ID, tracker.ChangeDiscussion{ID: "discussion_oversized", VersionID: current.ID, Body: strings.Repeat("customer-secret", operatortool.MaxResultBytes/7)})
		args = base
		args.Section = "discussion"
		for pages := range 20 {
			result, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args))
			if err != nil {
				var refusal *operatortool.RequestError
				if !errors.As(err, &refusal) || refusal.Code != "change_record_too_large" || strings.Contains(refusal.Message, "customer-secret") {
					t.Fatalf("unsafe oversized refusal: %v", err)
				}
				break
			}
			var value operatortool.ChangeResult
			if json.Unmarshal(result.Content, &value) != nil || value.Read.Sections["discussion"].Complete {
				t.Fatal("oversized evidence silently omitted")
			}
			args.Cursor = value.Read.Sections["discussion"].NextCursor
			if pages == 19 {
				t.Fatal("oversized evidence retried indefinitely")
			}
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?view=bounded&section=discussion&cursor="+url.QueryEscape(args.Cursor), f.token, nil)
		requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		if !strings.Contains(response.Body.String(), "change_record_too_large") || strings.Contains(response.Body.String(), "customer-secret") {
			t.Fatal("native oversized refusal leaked record")
		}
		args = base
		args.VersionID = version.ID
		for _, id := range []string{version.ID, current.ID} {
			args.VersionID = id
			result, err := executor.Execute(ctx, changeToolCall(operatortool.GetChangeVersion, args))
			var exact operatortool.ChangeResult
			if err != nil || json.Unmarshal(result.Content, &exact) != nil || exact.Version == nil || !reflect.DeepEqual(*exact.Version, versions[id]) {
				t.Fatalf("exact version lost full policy or receipt: %v", err)
			}
		}
		input := current.ChangeVersionInput
		input.HeadSHA, input.Source, input.Validation = strings.Repeat("7", 40), nil, nil
		response = performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "advance-after-cursor"}, ExpectedVersionID: current.ID, ChangeVersionInput: input})
		requireNativeStatus(t, response, http.StatusOK)
		args = base
		args.Cursor = fragmentCursor
		if _, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args)); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("fragment cursor crossed current-head change: %v", err)
		}
		args.Cursor = first.Read.Sections["versions"].NextCursor
		if _, err := executor.Execute(ctx, changeToolCall(operatortool.GetChange, args)); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("cursor crossed current-head change: %v", err)
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"?view=bounded&cursor="+url.QueryEscape(args.Cursor), f.token, nil)
		requireNativeStatus(t, response, http.StatusConflict)
	})

	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id=(SELECT id FROM api_tokens WHERE token_hash=?)", operatortool.ConnectionIdentity(ctx).CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(ctx, call); !errors.Is(err, operatortool.ErrAccessDenied) {
		t.Fatalf("lost grant replay: %v", err)
	}
}

// Catches forged artifact identity, token persistence/logging, permission loss
// during replay, read grants incorrectly requiring write, and unusable URLs.
func TestOperatorArtifactResults(t *testing.T) {
	for _, scenario := range []string{"valid replay", "historical revision", "viewer", "forged hash", "foreign item", "missing service", "expired", "lost grant", "revoked session"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			deadline := time.Now().Add(time.Hour)
			if scenario == "expired" {
				deadline = time.Now().Add(-time.Minute)
			}
			ref, _, _ := seedHostedArtifact(t, f, deadline)
			role, grant := "member", "write"
			if scenario == "viewer" {
				role, grant = "viewer", "read"
			}
			u := f.user(t, "download", role, "download@example.test", grant, "")
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			ctxs := make(chan context.Context, 1)
			f.service.echo.GET("/capture-authority", func(c echo.Context) error {
				ctxs <- operatortool.BindConnection(c.Request().Context(), "download-connection", "generic-client")
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			requireNativeStatus(t, f.request(t, u, http.MethodGet, "/capture-authority", nil), http.StatusOK)
			ctx := <-ctxs
			if scenario == "historical revision" {
				next := ref
				next.Revision++
				next.ManifestID = artifact.NewID("manifest")
				raw, _ := json.Marshal(next)
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO artifact_references(organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,reference_json) VALUES(?,?,?,?,?,?,?,?)`, next.OrganizationID, next.ProjectID, next.WorkItemID, next.ServiceID, next.ArtifactID, next.Revision, next.ManifestID, raw); err != nil {
					t.Fatal(err)
				}
			}
			args := operatortool.ChangeArguments{ProjectID: string(f.project), ItemID: ref.WorkItemID, ArtifactID: ref.ArtifactID, Revision: ref.Revision, SHA256: ref.SHA256, RequestID: "download"}
			ex := hubOperatorExecutor{f.service}
			switch scenario {
			case "forged hash":
				args.SHA256 = strings.Repeat("f", 64)
			case "foreign item":
				args.ItemID = "wi_foreign"
			case "missing service":
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE artifact_services SET binding_json='{}'"); err != nil {
					t.Fatal(err)
				}
			}
			call := changeToolCall(operatortool.ArtifactAccess, args)
			result, err := ex.Execute(ctx, call)
			if scenario == "forged hash" || scenario == "foreign item" || scenario == "missing service" || scenario == "expired" {
				if err == nil {
					t.Fatal("invalid download succeeded")
				}
				var count int
				if e := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM artifact_grants").Scan(&count); e != nil || count != 0 {
					t.Fatalf("denied grant effect=%d: %v", count, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var value operatortool.ChangeResult
			if err := json.Unmarshal(result.Content, &value); err != nil {
				t.Fatal(err)
			}
			if value.Access == nil || value.Access.Grant.Token == "" || !strings.HasSuffix(value.Access.ManifestURL, "/manifests/1") || !strings.HasSuffix(value.Access.ObjectURLTemplate, "/manifests/1/objects/{object_id}") {
				t.Fatalf("unusable result: %s", result.Content)
			}
			token := value.Access.Grant.Token
			if strings.Contains(logs.String(), token) || strings.Contains(string(value.Receipt), token) {
				t.Fatal("secret in audit/receipt")
			}
			var receipt string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE command_key='download'").Scan(&receipt); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(receipt, token) {
				t.Fatal("persisted token")
			}
			if time.Until(value.Access.Grant.ExpiresAt) > time.Minute {
				t.Fatal("grant exceeded existing TTL")
			}
			if scenario == "lost grant" {
				operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", u.identity.Subject)
			}
			if scenario == "revoked session" {
				operatorSQL(t, f, "UPDATE hosted_sessions SET revoked_at=?", formatHubTime(time.Now()))
			}
			replay, err := ex.Execute(ctx, call)
			if scenario == "lost grant" || scenario == "revoked session" {
				if !errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatalf("lost authority replay: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var repeated operatortool.ChangeResult
			if json.Unmarshal(replay.Content, &repeated) != nil || repeated.Access == nil || repeated.Access.Grant.Token == token {
				t.Fatal("replay must freshly authorize ephemeral access")
			}
		})
	}
}

func TestOperatorArchivedChangeReads(t *testing.T) {
	f := newChangeFixture(t, nil)
	first := f.publish(t, "first", "")
	second := f.publish(t, "second", first.ID)
	versionPath := f.path + "/versions/" + second.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "archive-review"}, Decision: "commented", Body: "Retained implementation review evidence"}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/checks", f.token, changeTestResult(second)), http.StatusOK)
	for i := range 3 {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprint("discussion-", i)}, VersionID: second.ID, Body: fmt.Sprint("Review discussion ", i)}), http.StatusOK)
	}
	at := f.service.config.now()
	f.service.config.now = func() time.Time { return at }
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	call := hostedContextProtocol(t, f.service, ctx, "stdio")
	read := func(t *testing.T, name, section, version, cursor string) []byte {
		t.Helper()
		args := map[string]any{"project_id": string(f.project.ID), "work_item_id": string(f.issue.WorkItemID), "change_id": f.change.ID}
		if name == operatortool.GetChange {
			args["limit"] = 1
		}
		if name == operatortool.WorkItem {
			args = map[string]any{"project_id": string(f.project.ID), "reference": string(f.issue.WorkItemID), "view": "recovery"}
		}
		if section != "" {
			args["section"] = section
		}
		if version != "" {
			args["version_id"] = version
		}
		if cursor != "" {
			args["cursor"] = cursor
		}
		reply := call("tools/call", name, args)
		var result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(reply.Result, &result) != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("%s: %s %s", name, reply.Result, reply.Error)
		}
		return []byte(result.Content[0].Text)
	}
	cases := []struct {
		name, section, version, cursor string
		hot                            []byte
	}{
		{operatortool.GetChange, "current", "", "", nil},
		{operatortool.GetChange, "versions", "", "", nil},
		{operatortool.GetChange, "reviews", "", "", nil},
		{operatortool.GetChange, "checks", "", "", nil},
		{operatortool.GetChange, "discussion", "", "", nil},
		{operatortool.GetChangeVersion, "", first.ID, "", nil},
		{operatortool.WorkItem, "", "", "", nil},
	}
	for i := range cases {
		cases[i].hot = read(t, cases[i].name, cases[i].section, cases[i].version, "")
	}
	var page operatortool.ChangeResult
	if err := json.Unmarshal(cases[4].hot, &page); err != nil {
		t.Fatal(err)
	}
	cursor := page.Read.Sections["discussion"].NextCursor
	if cursor == "" {
		t.Fatal("fixture did not exercise pagination")
	}
	cases = append(cases, struct {
		name, section, version, cursor string
		hot                            []byte
	}{operatortool.GetChange, "discussion", "", cursor, read(t, operatortool.GetChange, "discussion", "", cursor)})
	ref, data := publishTestSection(t, f.nativeFixture, string(f.issue.WorkItemID), "change", f.change.ID, 0, 0,
		archiveTestTable(t, f.service, "change_versions", "change_id=?", f.change.ID),
		archiveTestTable(t, f.service, "change_evidence", "change_id=?", f.change.ID))
	for _, table := range []string{"change_versions", "change_evidence"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER "+table+"_immutable"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE "+table+" SET record_json='{}' WHERE change_id=?", f.change.ID); err != nil {
			t.Fatal(err)
		}
	}
	fetches := 0
	f.service.config.ArchiveReader = func(_ context.Context, got attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
		fetches++
		if got != ref {
			t.Fatal("wrong archive")
		}
		return entryauthority.ArchiveResponse{Data: data}, nil
	}
	for i, test := range cases {
		t.Run(fmt.Sprint(i, "/", test.name, "/", test.section), func(t *testing.T) {
			fetches = 0
			assertArchiveGolden(t, test.hot, read(t, test.name, test.section, test.version, test.cursor))
			if fetches != 1 {
				t.Fatalf("fetches=%d", fetches)
			}
		})
	}
}
