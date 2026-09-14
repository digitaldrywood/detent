package hubserver

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The work-screen half of the shared contract fixtures: the native work
// endpoints the board, list and issue thread read (decisions section 12,
// "Work"). Only the shape is compared, so each value below carries one
// element per fixture element rather than the fixture's own data.

var workFixtureStamp = time.Date(2026, 9, 9, 10, 2, 44, 0, time.UTC)

// workFixtureIssue builds one item with the given collection sizes, since the
// fixture comparison requires equal array lengths.
func workFixtureIssue(labels, assignees, dependencies, blockers, references int) tracker.NativeIssue {
	priority := 1
	issue := tracker.NativeIssue{
		NativeReference: tracker.NativeReference{OrganizationID: "org_4d1f", ProjectID: "prj_8c1d", WorkItemID: "wi_3363", Number: 3363, Revision: 4, Profile: "native"},
		Title:           "Checkout lock renewal waits on a healthy handoff", Body: "The renewal path returns early.",
		State: "In Progress", Priority: &priority, Labels: []string{}, Assignees: []string{},
		Actor: tracker.Actor{Kind: "human", PrincipalID: "tok_7b21"}, CreatedAt: workFixtureStamp, UpdatedAt: workFixtureStamp,
		Dependencies:       []tracker.NativeWorkItemID{},
		Blockers:           []tracker.NativeDependency{},
		ExternalReferences: []tracker.ExternalReference{},
	}
	for range labels {
		issue.Labels = append(issue.Labels, "bug")
	}
	for range assignees {
		issue.Assignees = append(issue.Assignees, "michaelhvisser")
	}
	for range dependencies {
		issue.Dependencies = append(issue.Dependencies, "wi_3401")
	}
	for range blockers {
		issue.Blockers = append(issue.Blockers, tracker.NativeDependency{ID: "wi_3401", ProjectID: "prj_8c1d", State: "Todo"})
	}
	for range references {
		issue.ExternalReferences = append(issue.ExternalReferences, tracker.ExternalReference{Provider: "github", Kind: "issue", ID: "3363"})
	}
	return issue
}

func workFixtureAttempt() tracker.NativeAttempt {
	return tracker.NativeAttempt{
		NativeRunData: tracker.NativeRunData{
			Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implementer", Backend: "codex", Model: "gpt-6-astra"},
			MachineID: "mac_studio", RunnerID: "rnr_mac_studio", SessionID: "sess_90", LeaseID: "lease_6e19", FencingToken: 6,
			RunID: "run_1b8a", AttemptID: "att_07e3", PolicyID: "policy_0123", Outcome: "interrupted",
		},
		Status: "interrupted", StartedAt: workFixtureStamp, UpdatedAt: workFixtureStamp,
	}
}

// workFixtureAttemptDiff is the payload GET .../attempts/:attempt/diff
// projects. Only the shape is compared, so the patches carry one hunk each.
func workFixtureAttemptDiff() tracker.AttemptDiff {
	return tracker.AttemptDiff{
		ID: "diff_7b4c2f1a9e604d3fa1c8b5e207d43f61", AttemptID: "att_07e3",
		Producer: tracker.DiffProducer{
			Kind: tracker.DiffSourceAttempt, ID: "att_07e3", RunnerID: "rnr_mac_studio",
			LeaseID: "lease_6e19", FencingToken: 6,
		},
		Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2},
		BaseSHA:    "1111", HeadSHA: "a41f",
		Files: []tracker.AttemptDiffFile{
			{Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Patch: "diff --git"},
			{Path: "internal/hubserver/renewal.go", Status: tracker.DiffStatusAdded, Additions: 2, Patch: "diff --git"},
			{Path: ".env.local", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 1, Denied: true},
		},
		FileCount: 3, PatchBytes: 396, CreatedAt: workFixtureStamp,
	}
}

func workFixtureComment() tracker.NativeComment {
	return tracker.NativeComment{
		ID: "cmt_2f10", OrganizationID: "org_4d1f", ProjectID: "prj_8c1d", WorkItemID: "wi_3363", Revision: 2, Sequence: 7,
		Body: "Reproduced on the mac-studio runner.", Actor: tracker.Actor{Kind: "human", PrincipalID: "tok_7b21"},
		EditedBy: &tracker.Actor{Kind: "human", PrincipalID: "tok_7b21"}, CreatedAt: workFixtureStamp, UpdatedAt: workFixtureStamp,
	}
}

func workFixtureEvent(data tracker.CollaborationData) tracker.CollaborationEvent {
	return tracker.CollaborationEvent{
		ID: "evt_5a9c", OrganizationID: "org_4d1f", ProjectID: "prj_8c1d", AggregateType: "work_item", AggregateID: "wi_3363",
		AggregateSequence: 3, Type: "workflow.transitioned", SchemaVersion: 1, RecordedAt: workFixtureStamp,
		Actor: tracker.Actor{Kind: "human", PrincipalID: "tok_7b21"}, Data: data,
	}
}

func workFixtureChange() tracker.ChangeRequest {
	return tracker.ChangeRequest{
		ID: "change_9988", OrganizationID: "org_4d1f", ProjectID: "prj_8c1d", WorkItemID: "wi_3363",
		LinkedIssues: []tracker.NativeWorkItemID{"wi_3363"}, Title: "fix(checklock): renew waits on healthy handoffs",
		Body: "Blocks the renewal until the handoff is acknowledged.", CurrentVersion: "version_aabb", Revision: 2,
		CreatedAt: workFixtureStamp, UpdatedAt: workFixtureStamp,
	}
}

// nativeWorkFixtureCases binds the work fixtures to the payloads the native
// endpoints project.
func nativeWorkFixtureCases(t *testing.T) map[string]conversationFixtureCase {
	t.Helper()
	run := workFixtureAttempt().NativeRunData
	decodeInto := func(target func() any) func(t *testing.T, raw []byte) {
		return func(t *testing.T, raw []byte) {
			t.Helper()
			value := target()
			if err := json.Unmarshal(raw, value); err != nil {
				t.Fatalf("decode request: %v", err)
			}
		}
	}
	return map[string]conversationFixtureCase{
		"work-project.json": {value: tracker.NativeProject{
			ID: "prj_8c1d", OrganizationID: "org_4d1f", Name: "parable", Profile: "native", RequireDependencies: true,
			States: workFixtureStates(1, 3, 3, 2, 1, 1, 0),
		}, optional: []string{"states[].operator_only"}},
		"work-item.json": {value: workFixtureIssue(2, 1, 1, 1, 0)},
		"work-item-list.json": {value: tracker.Page[tracker.NativeIssue]{
			Items:      []tracker.NativeIssue{workFixtureIssue(2, 1, 1, 1, 0), workFixtureIssue(1, 0, 0, 0, 0), workFixtureIssue(0, 1, 0, 0, 1)},
			NextCursor: "eyJ2IjoyfQ",
		}, optional: []string{"items[].provenance"}},
		"work-comment-list.json": {value: tracker.Page[tracker.NativeComment]{
			Items: []tracker.NativeComment{workFixtureComment(), workFixtureComment()},
		}, extra: []string{"items[]"}, optional: []string{"items[].edited_by", "next_cursor"}},
		// The stored attempt diff the Diff surface draws (decisions section
		// 18.5). Three files, one per way a patch can be absent or present:
		// a modified file with its hunk, an added file, and a denied path that
		// keeps its counts and loses its contents.
		"work-attempt-diff.json": {value: workFixtureAttemptDiff()},
		"work-attempt-list.json": {value: tracker.Page[tracker.NativeAttempt]{
			Items: []tracker.NativeAttempt{workFixtureAttempt(), workFixtureAttempt()},
		}, extra: []string{"items[]"}, optional: []string{"items[].artifact_ids", "items[].checkpoint", "items[].outcome", "items[].handoff", "next_cursor"}},
		"work-history.json": {value: tracker.Page[tracker.CollaborationEvent]{
			Items: []tracker.CollaborationEvent{
				workFixtureEvent(tracker.CollaborationData{Revision: 5, FromState: "Todo", ToState: "In Progress", Reason: "user_requested"}),
				workFixtureEvent(tracker.CollaborationData{Run: &run}),
				workFixtureEvent(tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: "change_9988", VersionID: "version_aabb", HeadSHA: "a41f"}}),
			},
		}, extra: []string{"items[].data.run"}, optional: []string{"next_cursor"}},
		"work-change-list.json":   {value: []tracker.ChangeRequest{workFixtureChange()}},
		"work-change-detail.json": {value: workFixtureChangeDetail(), extra: []string{"", "versions[]", "versions[].policy", "versions[].review_policy", "reviews[]", "checks[]", "discussion[]", "external_snapshot"}},
		"work-error-conflict.json": {value: &nativeError{
			Code: "revision_conflict", Message: "Resource has changed", CurrentRevision: 6,
		}},
		"work-item-create-request.json": {decode: decodeInto(func() any { return &tracker.CreateIssue{} })},
		"work-item-patch-request.json":  {decode: decodeInto(func() any { return &tracker.UpdateIssue{} })},
		"work-transition-request.json":  {decode: decodeInto(func() any { return &tracker.Transition{} })},
	}
}

func workFixtureStates(transitions ...int) []tracker.NativeState {
	states := make([]tracker.NativeState, 0, len(transitions))
	for _, count := range transitions {
		state := tracker.NativeState{Name: "Todo", Transitions: []string{}}
		for range count {
			state.Transitions = append(state.Transitions, "Done")
		}
		states = append(states, state)
	}
	return states
}

func workFixtureChangeDetail() tracker.ChangeDetail {
	version := tracker.ChangeVersion{
		ChangeVersionInput: tracker.ChangeVersionInput{
			BaseSHA: "1111", HeadSHA: "a41f", MergeBaseSHA: "1111", Repository: "digitaldrywood/detent",
			Code:      tracker.ChangeArtifact{Kind: "code", URI: "artifact://a", SHA256: "9f86", Availability: "available"},
			Artifacts: []tracker.ChangeArtifact{}, RunID: "run_2c9a", AttemptID: "att_18f4", PolicyID: "policy_0123",
			External: &tracker.ChangeExternalReference{Provider: "github", ID: "3372", URL: "https://github.com/x/y/pull/3372"},
		},
		ID: "version_aabb", ChangeID: "change_9988", Number: 1, Policy: policy.Descriptor{},
		ReviewPolicy: tracker.ChangeReviewPolicy{RequiredChecks: []tracker.ChangeCheckSpec{{Name: "build"}}},
		Checks:       []tracker.ChangeCheckExpectation{{ChangeCheckSpec: tracker.ChangeCheckSpec{Name: "build"}, CheckRunID: "run_1"}},
		Actor:        tracker.Actor{Kind: "runner", PrincipalID: "tok_runner_1"},
		CreatedAt:    workFixtureStamp,
	}
	return tracker.ChangeDetail{
		Change: workFixtureChange(), Versions: []tracker.ChangeVersion{version},
		Reviews: []tracker.ChangeReview{{ID: "review_1", VersionID: "version_aabb", Decision: "approved"}},
		Checks: []tracker.ChangeCheck{{
			ChangeCheckResult: tracker.ChangeCheckResult{Evidence: []tracker.ChangeArtifact{}},
			VersionID:         "version_aabb", Actor: tracker.Actor{Kind: "runner", PrincipalID: "tok_runner_1"}, ReceivedAt: workFixtureStamp,
		}},
		Discussion: []tracker.ChangeDiscussion{{ID: "cmt_9f0a", VersionID: "version_aabb", Body: "Queued.", Actor: tracker.Actor{Kind: "runner", PrincipalID: "tok_runner_1"}, CreatedAt: workFixtureStamp}},
		Summary:    tracker.ChangeSummary{NativeReview: "pending", ExternalReview: "external_gate", Checks: "success", Status: "ready", Messages: []string{"Ready to merge."}},
	}
}
