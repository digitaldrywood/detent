package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeSequentialReleaseDeliveries(t *testing.T) {
	f := newPullRequestFixture(t, true)
	states := append(nativeFixtureStates(), tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"In Progress", "Done"}})
	rawStates, err := marshalNative(states)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", rawStates, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, 'Merging', 'Merging', 1, ?, ?)", f.project.ID, formatHubTime(f.now), formatHubTime(f.now)); err != nil {
		t.Fatal(err)
	}
	input := changeTestInput()
	_, directory := changeTestSourceCheckout(t, &input)
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Release Fixture", "-c", "user.email=release@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	frozen, firstSource := input.BaseSHA, input.HeadSHA
	git("branch", "-m", "develop")
	if err := os.WriteFile(filepath.Join(directory, "second.txt"), []byte("independent repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "second.txt")
	git("commit", "-m", "second repair")
	secondSource := git("rev-parse", "HEAD")
	git("checkout", "-b", "release/v1.2", frozen)
	git("checkout", "-b", "pick/first")
	git("cherry-pick", "-x", firstSource)
	firstPick := git("rev-parse", "HEAD")
	git("checkout", "-b", "pick/second", frozen)
	git("cherry-pick", "-x", secondSource)
	secondPick := git("rev-parse", "HEAD")
	connector, _, _, err := projectRepositoryBinding(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
	if err != nil || connector == nil {
		t.Fatalf("repository binding: %v", err)
	}
	repository := "https://github.com/" + connector.Repository
	readIssue := func(item tracker.NativeWorkItemID) tracker.NativeIssue {
		t.Helper()
		var issue tracker.NativeIssue
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item), f.token, nil), &issue)
		return issue
	}
	readDetail := func(path string) tracker.ChangeDetail {
		t.Helper()
		var detail tracker.ChangeDetail
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
		return detail
	}
	publish := func(path, key, expected string, base, head, policyID string, source bool) tracker.ChangeVersion {
		t.Helper()
		versionInput := changeTestInput()
		versionInput.Repository, versionInput.PolicyID = repository, policyID
		versionInput.BaseSHA, versionInput.MergeBaseSHA, versionInput.HeadSHA = base, base, head
		var bundle []byte
		if source {
			ref := key + "-" + head
			bundlePath := filepath.Join(directory, ref+".bundle")
			git("branch", ref, head)
			git("bundle", "create", bundlePath, ref, "^"+base)
			var err error
			bundle, err = os.ReadFile(bundlePath)
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "git", "-C", directory, "diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
			diff, err := command.Output()
			if err != nil {
				t.Fatal(err)
			}
			versionInput.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: base, HeadSHA: head, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: tracker.ChangeSourceDigest(diff), Bytes: int64(len(bundle))}
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedVersionID: expected, ChangeVersionInput: versionInput, SourceBundle: bundle})
		requireNativeStatus(t, response, http.StatusOK)
		var version tracker.ChangeVersion
		decodeHubResponse(t, response, &version)
		return version
	}
	approve := func(path string, version tracker.ChangeVersion) {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "review-" + version.ID}, Decision: "approved"}), http.StatusOK)
	}
	audit := func(change string, issue tracker.NativeWorkItemID, version tracker.ChangeVersion, pr int, historical bool) {
		t.Helper()
		review := tracker.SecurityAuditReview{SessionID: 42, Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: version.ID, ProjectID: string(f.project.ID), IssueID: string(issue), Repository: repository, PRNumber: pr, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: f.now, StartedAt: f.now, CompletedAt: f.now.Add(time.Second), OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
		if historical {
			review.Run.ReviewerVersion = "2"
			review.Run.ReviewerDigest = "18feae8d1c5b5a50cbcfdd9feb1280302a3c888881b7facf4200d6af3c3ee761"
			review.Run.Summary = "No findings in the activation diff; referenced helpers were not supplied."
		}
		raw, err := marshalNative(tracker.ChangeReview{VersionID: version.ID, Decision: "approved", SecurityAudit: &review})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'review', ?)", change, version.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	land := func(key string, version tracker.ChangeVersion, baseRef, merge string) *tracker.LandChangeVersion {
		t.Helper()
		return &tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, MergeSHA: merge, BaseRef: baseRef, Method: "squash", Receipt: &tracker.NativeLandingReceipt{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, MergeSHA: merge}}
	}
	sourceIssues := []tracker.NativeIssue{f.issue, f.create(t, "second source repair")}
	sourceChanges := []tracker.ChangeRequest{f.change, {}}
	sourceHeads := []string{firstSource, secondSource}
	for i := range sourceIssues {
		path := f.base + "/work-items/" + string(sourceIssues[i].WorkItemID) + "/changes"
		if i > 0 {
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "second-source"}, Title: "second source"})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &sourceChanges[i])
		}
		path += "/" + sourceChanges[i].ID
		base := frozen
		if i > 0 {
			base = firstSource
		}
		version := publish(path, fmt.Sprintf("source-%d", i), "", base, sourceHeads[i], hubTestPolicy().ID, false)
		approve(path, version)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, land("source-land", version, "develop", sourceHeads[i])), http.StatusOK)
		audit(sourceChanges[i].ID, sourceIssues[i].WorkItemID, version, 0, true)
	}
	descriptor := hubTestPolicy()
	descriptor.Gates.SecurityAudit = true
	descriptor = descriptor.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
	rules := f.rules
	rules.PolicyID = descriptor.ID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "release-rules"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
	verifier := &releaseVerifierFixture{verify: func(input tracker.ReleasePickInput) error {
		if git("rev-parse", input.BaseRef) != input.BaseSHA || git("rev-parse", input.HeadRef) != input.HeadSHA || git("rev-parse", input.HeadSHA+"^") != input.BaseSHA || git("merge-base", input.BaseSHA, "develop") != input.FrozenSHA || !strings.Contains(git("log", "-1", "--format=%B", input.HeadSHA), "(cherry picked from commit "+input.SourceMergeSHA+")") {
			return ErrReleasePickMismatch
		}
		return nil
	}}
	publisher := &releasePublisherFixture{releaseVerifierFixture: verifier}
	f.service.config.ReconcileBackend = publisher
	(&releaseDeliveryFixture{pullRequestFixture: f, request: tracker.AssociateReleasePick{ReleasePickInput: tracker.ReleasePickInput{Repository: repository}}}).checkoutOnly(t)
	deliveries := make([]*releaseDeliveryFixture, 2)
	versions := make([]tracker.ChangeVersion, 2)
	paths := make([]string, 2)
	for i, head := range []string{firstPick, secondPick} {
		issue := f.create(t, fmt.Sprintf("release delivery %d", i))
		itemPath := f.base + "/work-items/" + string(issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "dependency"}, ExpectedRevision: issue.Revision, RelatedWorkItemID: sourceIssues[i].WorkItemID, Operation: "add"}), http.StatusOK)
		issue = readIssue(issue.WorkItemID)
		request := tracker.AssociateReleasePick{Mutation: tracker.Mutation{IdempotencyKey: "associate"}, ExpectedRevision: issue.Revision, ReleasePickInput: tracker.ReleasePickInput{SourceIssueID: sourceIssues[i].WorkItemID, SourceChangeID: sourceChanges[i].ID, SourceMergeSHA: sourceHeads[i], Repository: repository, PRNumber: 7 + i, HeadRef: []string{"pick/first", "pick/second"}[i], HeadSHA: head, BaseRef: "release/v1.2", BaseSHA: frozen, FrozenSHA: frozen}}
		df := &releaseDeliveryFixture{pullRequestFixture: f, delivery: issue, request: request, verifier: verifier, itemPath: itemPath}
		verifier.input = request.ReleasePickInput
		d := df.associate(t, request)
		paths[i] = itemPath + "/changes/" + d.ChangeID
		versions[i] = publish(paths[i], "release-version", "", frozen, head, descriptor.ID, true)
		approve(paths[i], versions[i])
		audit(d.ChangeID, issue.WorkItemID, versions[i], request.PRNumber, false)
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Merging') WHERE native_id = ?", f.project.ID, issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		deliveries[i] = df
	}
	first := deliveries[0]
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, paths[0]+"/versions/"+versions[0].ID+"/landing", f.token, land("release-land", versions[0], "release/v1.2", firstPick)), http.StatusOK)
	git("checkout", "release/v1.2")
	git("merge", "--ff-only", "pick/first")
	second := deliveries[1]
	sourcePath := f.base + "/work-items/" + string(sourceIssues[1].WorkItemID) + "/changes/" + sourceChanges[1].ID
	sourceBefore := readDetail(sourcePath)
	sourceAudit := sourceBefore.Reviews[len(sourceBefore.Reviews)-1].SecurityAudit.Run
	if evaluation := securityaudit.Evaluate(sourceAudit, nil, securityaudit.Key{ProjectID: string(f.project.ID), Repository: repository, BaseSHA: sourceAudit.BaseSHA, HeadSHA: secondSource}, "", []string{"p1", "p2"}); evaluation.Reason != securityaudit.ReasonReviewerMismatch {
		t.Fatalf("historical audit unexpectedly meets current reviewer policy: %+v", evaluation)
	}
	verifier.input = second.request.ReleasePickInput
	if err := verifier.VerifyReleasePick(t.Context(), second.request.ReleasePickInput); !errors.Is(err, ErrReleasePickMismatch) {
		t.Fatalf("old release base remained valid: %v", err)
	}
	git("checkout", "pick/second")
	git("reset", "--hard", firstPick)
	git("cherry-pick", "-x", secondSource)
	refreshedHead := git("rev-parse", "HEAD")
	request := second.request
	request.IdempotencyKey = "refresh"
	request.ExpectedRevision = readIssue(second.delivery.WorkItemID).Revision
	request.BaseSHA, request.HeadSHA = firstPick, refreshedHead
	verifier.input = request.ReleasePickInput
	publication := tracker.PublishReleasePick{Mutation: tracker.Mutation{IdempotencyKey: "refresh-publication"}, ExpectedRevision: request.ExpectedRevision, ReleasePickInput: request.ReleasePickInput}
	for range 2 {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, second.itemPath+"/release-pick/publication", f.token, publication), http.StatusOK)
	}
	prior := readDetail(paths[1])
	second.associate(t, request)
	second.associate(t, request)
	current := readDetail(paths[1])
	if !reflect.DeepEqual(sourceBefore, readDetail(sourcePath)) || !readIssue(sourceIssues[1].WorkItemID).Terminal {
		t.Fatal("refresh changed the landed source or its historical review evidence")
	}
	if current.Change.CurrentVersion != "" || len(current.Versions) != 1 || len(current.Reviews) != len(prior.Reviews) || len(current.Checks) != len(prior.Checks) || current.Versions[0].HeadSHA != secondPick || readIssue(second.delivery.WorkItemID).State != "In Progress" {
		t.Fatalf("refresh lost immutable evidence or retained approval: %+v", current)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, paths[1]+"/versions/"+versions[1].ID+"/landing", f.token, land("old-land", versions[1], "release/v1.2", secondPick)), http.StatusUnprocessableEntity)
	version := publish(paths[1], "refreshed-version", "", firstPick, refreshedHead, descriptor.ID, true)
	landingPath := paths[1] + "/versions/" + version.ID + "/landing"
	landing := land("refreshed-land", version, "release/v1.2", refreshedHead)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, landingPath, f.token, landing), http.StatusConflict)
	approve(paths[1], version)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, landingPath, f.token, landing), http.StatusConflict)
	audit(current.Change.ID, second.delivery.WorkItemID, version, 8, true)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, landingPath, f.token, landing), http.StatusConflict)
	audit(current.Change.ID, second.delivery.WorkItemID, version, 8, false)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, landingPath, f.token, landing), http.StatusOK)
	git("checkout", "release/v1.2")
	git("merge", "--ff-only", "pick/second")
	if git("rev-list", "--count", frozen+"..release/v1.2") != "2" || git("show", "release/v1.2:second.txt") != "independent repair" || !readIssue(first.delivery.WorkItemID).Terminal || !readIssue(second.delivery.WorkItemID).Terminal || publisher.creates != 0 {
		t.Fatal("sequential deliveries did not finish on the frozen branch")
	}
	request.ExpectedRevision = readIssue(second.delivery.WorkItemID).Revision
	request.IdempotencyKey = "terminal-refresh"
	request.BaseSHA, request.HeadSHA = strings.Repeat("1", 40), strings.Repeat("2", 40)
	verifier.verify = nil
	verifier.input = request.ReleasePickInput
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, second.itemPath+"/release-pick", f.token, request), http.StatusUnprocessableEntity)
}
