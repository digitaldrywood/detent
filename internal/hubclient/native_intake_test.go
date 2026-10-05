package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeMachineIntakeAuthorityAndFingerprint(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	states := append(hubserver.HostedProjectStates(), tracker.NativeState{Name: "Backlog", OperatorOnly: true, Transitions: []string{"Todo", "Blocked", "Done"}})
	for i := range states {
		if states[i].Name == "Blocked" {
			states[i].OperatorOnly = true
		}
	}
	h := newNativeChangeHubWithStates(t, "Human Review", states)
	identityPath := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(identityPath, h.admin.client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, ID: file.Identity.MachineID, Hostname: "native-intake", DisplayName: "Native intake runner", Capacity: 1, Version: "test"}
	if _, err := EnrollRunner(t.Context(), identityPath, h.organization, enrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: identityPath})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	store, ok := any(conn).(intake.IssueStore)
	if !ok {
		t.Fatal("native worker has no existing machine intake owner")
	}
	h.native = native
	h.scheduler, err = NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: machine, HeartbeatInterval: time.Second, LeaseTTL: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	source := h.createInProgress(t, "Complete implementation before delegated acceptance")
	worker := nativeIntakeWorker{started: make(chan runner.RunRequest, 1)}
	orch, err := orchestrator.New(orchestrator.Config{
		Project: scheduler.ProjectCandidate{ID: "local"}, Policy: h.descriptor,
		PollInterval: time.Hour, MaxConcurrentAgents: 1,
		ActiveStates: []string{"In Progress"}, ObservedStates: []string{"Backlog", "Blocked"}, TerminalStates: []string{"Done"},
	}, orchestrator.Dependencies{Connector: conn, Scheduling: h.scheduler, Runner: worker, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- orch.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	var workerRequest runner.RunRequest
	select {
	case workerRequest = <-worker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("native source was not dispatched")
	}
	if workerRequest.Issue.ID != source.ID {
		t.Fatalf("dispatched source = %s, want %s", workerRequest.Issue.ID, source.ID)
	}
	ctx, stop, err := workerRequest.Execution.Guard(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for range 22 {
		if _, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), Title: "Earlier work", State: "Backlog"}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := workerRequest.AgentToolHandler(ctx, runner.AgentToolCall{Name: "file_machine_issue", Arguments: json.RawMessage(`{"title":"Verify integrated pool","body":"Pending real two-Sprite acceptance.\n<!-- acceptance-owner -->","fingerprint":"pool-live-acceptance","labels":["acceptance"],"priority":2}`)})
	if err != nil || !result.Success {
		t.Fatalf("worker filing = %+v, %v", result, err)
	}
	created, matched, err := store.FindIntakeIssue(t.Context(), "<!-- acceptance-owner -->")
	if err != nil || !matched || created.ID == "" {
		t.Fatalf("worker follow-up = %#v, error = %v", created, err)
	}
	readTools, readHandler := workerRequest.Execution.(runner.ToolExecution).AgentTools()
	for _, tool := range readTools {
		if tool.Name == operatortool.FileIssue || tool.Name == operatortool.EditItem {
			t.Fatalf("worker gained operator mutation %s", tool.Name)
		}
	}
	readArgs, err := json.Marshal(map[string]string{"project_id": string(h.project), "reference": created.ID})
	if err != nil {
		t.Fatal(err)
	}
	readResult, err := readHandler(ctx, runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: readArgs})
	var view operatortool.WorkReadResult[operatortool.NativeItem]
	if err != nil || !readResult.Success || json.Unmarshal([]byte(readResult.Content), &view) != nil || view.Data.Priority == nil || *view.Data.Priority != 1 || view.Data.ProjectID != h.project || view.Data.State != "Backlog" {
		t.Fatalf("worker structured High read = %s, %v", readResult.Content, err)
	}
	current, err := native.Issue(t.Context(), tracker.NativeWorkItemID(created.ID))
	if err != nil || current.State != "Backlog" {
		t.Fatalf("follow-up entered a dispatchable first workflow state: %#v, %v", current, err)
	}
	found, matched, err := store.FindIntakeIssue(t.Context(), "<!-- acceptance-owner -->")
	if err != nil || !matched || found.ID != created.ID {
		t.Fatalf("paginated match = %#v, %t, %v", found, matched, err)
	}
	origin := issueorigin.Origin{Kind: "worker", Source: "attempt-2", Fingerprint: "pool-live-acceptance"}
	draft := intake.IssueDraft{Title: "Verify integrated pool", Body: issueorigin.Stamp("Second occurrence", origin), Priority: new(4)}
	reused, err := store.CreateIntakeIssue(ctx, draft)
	if err != nil || !reused.Reused || reused.ID != created.ID {
		t.Fatalf("same fingerprint = %#v, %v", reused, err)
	}
	comments, err := native.Comments(t.Context(), tracker.NativeWorkItemID(created.ID), "")
	if err != nil || len(comments.Items) != 1 || !strings.Contains(comments.Items[0].Body, "## New machine occurrence") {
		t.Fatalf("occurrence = %#v, %v", comments, err)
	}
	updated, err := store.UpdateIntakeIssue(ctx, created.ID, intake.IssueDraft{Title: "Verify deployed pool", Body: "Updated criteria", Labels: []string{"deployment"}})
	if err != nil {
		t.Fatal(err)
	}
	updatedOrigin, stamped := issueorigin.Parse(updated.Body)
	if !stamped || updatedOrigin.Source != strconv.FormatInt(workerRequest.WorkAttemptID, 10) || updatedOrigin.Fingerprint != origin.Fingerprint {
		t.Fatalf("origin replaced: %#v", updatedOrigin)
	}
	current, err = native.Issue(t.Context(), tracker.NativeWorkItemID(created.ID))
	if err != nil || !slices.Equal(current.Labels, []string{"acceptance", "deployment"}) || current.Priority == nil || *current.Priority != 1 {
		t.Fatalf("reuse changed metadata or downgraded High = %#v, %v", current, err)
	}
	if err := store.SetIntakeIssueState(ctx, created.ID, "Backlog"); err != nil {
		t.Fatal(err)
	}
	for _, request := range []tracker.CreateIssue{
		{Mutation: nativeMutationKey(), Title: "Unfenced host creation", Body: draft.Body, State: "Backlog"},
		{Mutation: nativeMutationKey(), Title: "Unstamped host creation", State: "Backlog"},
	} {
		if _, err := native.CreateIssue(t.Context(), request); err == nil {
			t.Fatal("generic worker creation bypassed operator-only Backlog")
		}
	}
	h.scheduler.mu.Lock()
	claim := h.scheduler.nativeClaims[source.ID]
	h.scheduler.mu.Unlock()
	for _, mutation := range []tracker.Mutation{
		{IdempotencyKey: "wrong-fence", LeaseID: claim.lease.ID, FencingToken: claim.lease.FencingToken + 1},
		{IdempotencyKey: "unknown-lease", LeaseID: "lease_unknown", FencingToken: claim.lease.FencingToken},
	} {
		if _, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: mutation, Title: draft.Title, Body: draft.Body, State: "Backlog"}); err == nil {
			t.Fatal("invalid source claim reused machine intake")
		}
	}
	request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "same-intake-occurrence", LeaseID: claim.lease.ID, FencingToken: claim.lease.FencingToken}, Title: draft.Title, Body: draft.Body, State: "Backlog", Priority: new(0)}
	imported := request
	imported.IdempotencyKey = "forbidden-intake-import"
	imported.Provenance = &tracker.Provenance{Provider: "github", ExternalID: "private-source", AuthorID: "source-author", CreatedAt: time.Now(), UpdatedAt: time.Now(), ObservedAt: time.Now()}
	if _, err := native.CreateIssue(t.Context(), imported); err == nil {
		t.Fatal("fingerprint reuse bypassed imported provenance authority")
	}
	for range 2 {
		replayed, err := native.CreateIssue(t.Context(), request)
		if err != nil || string(replayed.WorkItemID) != created.ID {
			t.Fatalf("intake replay = %#v, %v", replayed, err)
		}
	}
	comments, err = native.Comments(t.Context(), tracker.NativeWorkItemID(created.ID), "")
	if err != nil || len(comments.Items) != 2 {
		t.Fatalf("intake replay repeated occurrence: %#v, %v", comments, err)
	}
	current, err = h.admin.Issue(t.Context(), tracker.NativeWorkItemID(created.ID))
	if err != nil || current.Priority == nil || *current.Priority != 0 {
		t.Fatalf("intake replay did not retain Urgent: %#v, %v", current, err)
	}
	history, err := native.History(t.Context(), current.WorkItemID, "")
	if err != nil {
		t.Fatal(err)
	}
	priorityEdits := 0
	for _, event := range history.Items {
		if event.Type == "issue.edited" && slices.Contains(event.Data.Fields, "priority") {
			priorityEdits++
			if !slices.Equal(event.Data.Fields, []string{"priority"}) {
				t.Fatalf("intake priority edit changed other fields: %#v", event)
			}
		}
	}
	if priorityEdits != 1 {
		t.Fatalf("replayed intake priority edits = %d, want 1", priorityEdits)
	}
	current, err = h.admin.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, State: "Blocked", Reason: "user_requested"})
	if err != nil {
		t.Fatal(err)
	}
	reuseCases := []struct {
		name     string
		existing *int
		rank     *int
		want     *int
	}{
		{name: "raise unset", rank: new(2), want: new(1)},
		{name: "raise normal", existing: new(2), rank: new(2), want: new(1)},
		{name: "raise low", existing: new(3), rank: new(2), want: new(1)},
		{name: "preserve urgent", existing: new(0), rank: new(2), want: new(0)},
		{name: "preserve high", existing: new(1), rank: new(4), want: new(1)},
		{name: "same high", existing: new(1), rank: new(2), want: new(1)},
		{name: "omission preserves urgent", existing: new(0), want: new(0)},
		{name: "omission preserves unset"},
	}
	for _, test := range reuseCases {
		t.Run(test.name, func(t *testing.T) {
			priority := tracker.SetPriority(test.existing)
			if test.existing == nil {
				priority = tracker.ClearPriority()
			}
			before, err := h.admin.UpdateIssue(t.Context(), current.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: current.Revision, Priority: priority})
			if err != nil {
				t.Fatal(err)
			}
			draft.Priority = test.rank
			reused, err := store.CreateIntakeIssue(ctx, draft)
			if err != nil || !reused.Reused || reused.ID != created.ID {
				t.Fatalf("fingerprint reuse = %#v, %v", reused, err)
			}
			current, err = h.admin.Issue(t.Context(), before.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			if (current.Priority == nil) != (test.want == nil) || test.want != nil && *current.Priority != *test.want {
				t.Fatalf("priority = %v, want %v", current.Priority, test.want)
			}
			if current.State != "Blocked" || current.Title != before.Title || current.Body != before.Body || !slices.Equal(current.Labels, before.Labels) || !slices.Equal(current.Assignees, before.Assignees) || current.Archived != before.Archived {
				t.Fatalf("reuse changed held item content: before=%#v, after=%#v", before, current)
			}
			wantRevision := before.Revision
			if test.rank != nil && (test.existing == nil || *test.rank-1 < *test.existing) {
				wantRevision++
			}
			if current.Revision != wantRevision {
				t.Fatalf("reuse revision = %d, want %d", current.Revision, wantRevision)
			}
		})
	}
	for _, test := range []struct {
		name string
		rank *int
		want *int
	}{
		{name: "legacy omission"},
		{name: "urgent", rank: new(1), want: new(0)},
		{name: "high", rank: new(2), want: new(1)},
		{name: "normal", rank: new(3), want: new(2)},
		{name: "low", rank: new(4), want: new(3)},
	} {
		t.Run(test.name, func(t *testing.T) {
			draft := intake.IssueDraft{Title: test.name, Body: issueorigin.Stamp("Priority: High.", issueorigin.Origin{Kind: "worker", Fingerprint: "rank-" + test.name}), Priority: test.rank}
			created, err := store.CreateIntakeIssue(ctx, draft)
			if err != nil || created.Reused {
				t.Fatalf("rank creation = %#v, %v", created, err)
			}
			issue, err := native.Issue(t.Context(), tracker.NativeWorkItemID(created.ID))
			if err != nil || issue.State != "Backlog" || issue.ProjectID != h.project || (issue.Priority == nil) != (test.want == nil) || test.want != nil && *issue.Priority != *test.want {
				t.Fatalf("rank mapping = %#v, %v", issue, err)
			}
		})
	}
	candidates, err := conn.FetchCandidateIssues(t.Context())
	if err != nil || len(candidates) != 1 || candidates[0].ID != source.ID {
		t.Fatalf("priority admitted a Backlog or held follow-up: %#v, %v", candidates, err)
	}
	for _, rank := range []int{-1, 0, 5} {
		draft.Priority = &rank
		if _, err := store.CreateIntakeIssue(ctx, draft); err == nil {
			t.Fatalf("native intake accepted creation rank %d", rank)
		}
	}
	draft.Priority = new(2)
	var foreignProject tracker.NativeProject
	if err := h.admin.client.request(t.Context(), http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/projects", map[string]any{"name": "foreign", "idempotency_key": "foreign-intake", "states": states}, &foreignProject); err != nil {
		t.Fatal(err)
	}
	foreign, err := client.Native(h.organization, foreignProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreignConnector, err := NewNativeConnector(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignStore, ok := any(foreignConnector).(intake.IssueStore)
	if !ok {
		t.Fatal("foreign connector lost the shared intake interface")
	}
	_, err = foreignStore.CreateIntakeIssue(t.Context(), draft)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("foreign authority = %v", err)
	}
	readOnlyPath := filepath.Join(t.TempDir(), "private", "identity.json")
	readOnlyFile, err := runnerauth.Initialize(readOnlyPath, h.admin.client.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	readOnlyEnrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: readOnlyFile.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	machine.ID = readOnlyFile.Identity.MachineID
	if _, err := EnrollRunner(t.Context(), readOnlyPath, h.organization, readOnlyEnrollment.Token, machine); err != nil {
		t.Fatal(err)
	}
	readOnlyClient, err := New(Config{URL: readOnlyFile.HubURL, IdentityFile: readOnlyPath})
	if err != nil {
		t.Fatal(err)
	}
	readOnlyNative, err := readOnlyClient.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyConnector, err := NewNativeConnector(readOnlyNative)
	if err != nil {
		t.Fatal(err)
	}
	readOnlyStore, ok := any(readOnlyConnector).(intake.IssueStore)
	if !ok {
		t.Fatal("read-only connector lost the shared intake interface")
	}
	origin.Fingerprint = "missing-collaboration-permission"
	draft.Body = issueorigin.Stamp("Cannot delegate without filing authority", origin)
	_, err = readOnlyStore.CreateIntakeIssue(t.Context(), draft)
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("read-only runner published follow-up: %v", err)
	}
	if err := native.Release(t.Context(), claim.lease, "released"); err != nil {
		t.Fatal(err)
	}
	origin.Fingerprint = "pool-live-acceptance"
	draft.Body = issueorigin.Stamp("Released source must not add an occurrence", origin)
	if _, err := store.CreateIntakeIssue(ctx, draft); err == nil {
		t.Fatal("released source claim reused machine intake")
	}
	comments, err = native.Comments(t.Context(), tracker.NativeWorkItemID(created.ID), "")
	if err != nil || len(comments.Items) != 2+len(reuseCases) {
		t.Fatalf("refused intake wrote occurrence: %#v, %v", comments, err)
	}
	origin.Fingerprint = "released-new-follow-up"
	draft.Body = issueorigin.Stamp("Released source must not create follow-up", origin)
	if _, err := store.CreateIntakeIssue(ctx, draft); err == nil {
		t.Fatal("released source claim created machine intake")
	}
}

type nativeIntakeWorker struct {
	started chan runner.RunRequest
}

func (w nativeIntakeWorker) Run(ctx context.Context, request runner.RunRequest) (runner.RunResult, error) {
	w.started <- request
	<-ctx.Done()
	return runner.RunResult{}, ctx.Err()
}

type intakeRepositoryBackend struct{}

func (intakeRepositoryBackend) Reconcile(context.Context, hubserver.ReconcileRequest) (hubserver.ReconcileSnapshot, error) {
	return hubserver.ReconcileSnapshot{Repository: hubserver.RepositorySource{NodeID: "R_source", Owner: "acme", Name: "orders", UpdatedAt: time.Now().UTC()}}, nil
}

func newLinkedChangeHub(t *testing.T) (*nativeChangeHub, tracker.NativeIssue) {
	t.Helper()
	h := newNativeChangeHubWithStates(t, "Human Review", hubserver.HostedProjectStates(), intakeRepositoryBackend{})
	if err := h.admin.client.request(t.Context(), http.MethodPost, h.admin.base()+"/onboarding/repository", map[string]any{"idempotency_key": "attach", "expected_revision": "1", "repository": "acme/orders"}, nil); err != nil {
		t.Fatal(err)
	}
	issue, err := h.admin.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: nativeMutationKey(), GitHubIssueURL: "https://github.com/acme/orders/issues/12", State: "In Progress"})
	if err != nil {
		t.Fatal(err)
	}
	return h, issue
}

func intakeSnapshot() tracker.GitHubIssueSnapshot {
	now := time.Now().UTC()
	prov := tracker.Provenance{Provider: "github", ExternalID: "I_source", AuthorID: "source-author", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute), ObservedAt: now}
	comment := prov
	comment.ExternalID = "C_source"
	return tracker.GitHubIssueSnapshot{URL: "https://github.com/acme/orders/issues/12", Title: "Imported execution task", Body: "Complete implementation context", Provenance: prov, Comments: []tracker.GitHubIssueComment{{Body: "Useful source discussion", Provenance: comment}}}
}

func TestNativeSourceIntakeRetryBeforeDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	for _, failure := range []struct {
		name    string
		err     error
		missing bool
		persist bool
	}{
		{name: "missing credential", err: githubconnector.ErrMissingToken, missing: true},
		{name: "private authorization", err: githubconnector.ErrAuthenticationFailed},
		{name: "inaccessible issue", err: githubconnector.ErrNotFound},
		{name: "rate limit", err: githubconnector.ErrRateLimited},
		{name: "partial fetch", err: githubconnector.ErrInvalidResponse},
		{name: "persistence failure", persist: true},
	} {
		t.Run(failure.name, func(t *testing.T) {
			h, issue := newLinkedChangeHub(t)
			var calls int
			if !failure.missing {
				h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
					calls++
					if failure.persist {
						return intakeSnapshot(), nil
					}
					return tracker.GitHubIssueSnapshot{}, failure.err
				}
			}
			h.failChanges.failIntake.Store(failure.persist)
			request := orchestrator.SchedulingRequest{ProjectID: "local", Repository: "acme/orders", Policy: h.descriptor, WorkflowStates: []string{"In Progress"}}
			items, err := h.scheduler.FetchCandidateIssues(t.Context(), request)
			if !errors.Is(err, orchestrator.ErrSchedulingUnavailable) || len(items) != 0 || !strings.Contains(err.Error(), "source intake") {
				t.Fatalf("dispatch = %#v, error = %v", items, err)
			}
			if _, stop, guardErr := h.scheduler.RunExecution(string(issue.WorkItemID)).Guard(t.Context()); guardErr == nil {
				stop()
				t.Fatal("failed intake granted execution authority")
			}
			stored, err := h.admin.Issue(t.Context(), issue.WorkItemID)
			if err != nil || stored.LinkedSource.Status != "pending" || stored.Body != "" || stored.State != "In Progress" {
				t.Fatalf("failed intake changed issue: %#v, error = %v", stored, err)
			}
			// The released failed claim can be claimed immediately using existing
			// scheduling. No issue lane move or recovery mechanism is needed.
			h.failChanges.failIntake.Store(false)
			h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
				calls++
				return intakeSnapshot(), nil
			}
			items, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(items) != 1 || items[0].Description != "Complete implementation context" {
				t.Fatalf("retry = %#v, error = %v", items, err)
			}
			claim := h.scheduler.nativeClaims[string(issue.WorkItemID)]
			if len(claim.recovery.Discussion) != 1 || claim.recovery.Issue.LinkedSource.Status != "complete" || claim.recovery.Discussion[0].Provenance.ExternalID != "C_source" {
				t.Fatalf("recovery = %#v", claim.recovery)
			}
			if err := h.native.Release(t.Context(), claim.lease, "released"); err != nil {
				t.Fatal(err)
			}
			completedCalls := calls
			// Credential removal after successful intake must have no effect on
			// another run, comments, Workpad edits, or native source reads.
			h.scheduler.githubIntake = func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
				calls++
				return tracker.GitHubIssueSnapshot{}, errors.New("must never be called after intake")
			}
			items, err = h.scheduler.FetchCandidateIssues(t.Context(), request)
			if err != nil || len(items) != 1 {
				t.Fatalf("second run = %#v, error = %v", items, err)
			}
			if err := h.connector.CreateComment(t.Context(), items[0].ID, "## Codex Workpad\nNative progress"); err != nil {
				t.Fatal(err)
			}
			if err := h.connector.UpdateIssueBody(t.Context(), items[0].ID, "Native task edits"); err != nil {
				t.Fatal(err)
			}
			if _, err := h.native.Recovery(t.Context(), issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if calls != completedCalls {
				t.Fatalf("GitHub source calls after intake = %d", calls-completedCalls)
			}
		})
	}
}

func TestNativeEditsDuringSourceFetch(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	h, issue := newLinkedChangeHub(t)
	started, proceed := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	h.scheduler.githubIntake = func(ctx context.Context, _ string) (tracker.GitHubIssueSnapshot, error) {
		close(started)
		select {
		case <-proceed:
		case <-ctx.Done():
			return tracker.GitHubIssueSnapshot{}, ctx.Err()
		}
		return intakeSnapshot(), nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := h.scheduler.FetchCandidateIssues(ctx, orchestrator.SchedulingRequest{ProjectID: "local", Repository: "acme/orders", Policy: h.descriptor, WorkflowStates: []string{"In Progress"}})
		finished <- err
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("claim ended before source fetch: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	title, body := "Human task title", "Human task body"
	_, editErr := h.admin.UpdateIssue(t.Context(), issue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision, Title: &title, Body: &body})
	close(proceed)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if editErr != nil {
		t.Fatal(editErr)
	}
	recovery := h.scheduler.nativeClaims[string(issue.WorkItemID)].recovery
	if recovery.Issue.Title != title || recovery.Issue.Body != body || recovery.Issue.LinkedSource.Snapshot.Body != "Complete implementation context" || len(recovery.Discussion) != 1 {
		t.Fatalf("native edits or complete source evidence lost during fetch: %#v", recovery)
	}
}
