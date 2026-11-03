package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestPostgresRunnerPublishesReviewsAndCompletesSuccessfully(t *testing.T) {
	f := newRunnerPGFixture(t, func(p *policy.Descriptor) {
		p.Gates.AutomatedReview = gate.AutomatedReviewRequired
		p.Gates.Validator = true
	})
	native, ctx := f.client(t, 0)
	sessions := api.NewRunnerSessions(native)
	session := func(role string) int64 {
		t.Helper()
		id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(f.leases[0].WorkItemID), StartedAt: time.Now().UTC(), AgentRole: role, Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: time.Now().UTC(), FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	sourceSession := session("code")
	change, err := native.CreateChange(ctx, f.leases[0].WorkItemID, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "review_change"}, Title: "Example source"})
	if err != nil {
		t.Fatal(err)
	}
	source := f.data[0]
	source.Sequence = 2
	source.Outcome = "succeeded"
	source.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: time.Now().UTC(), Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: sourceSession, Status: "completed"}}
	if err := native.AppendEvent(ctx, f.leases[0].WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "source_finished"}, Type: "run.finished", SchemaVersion: 1, Data: source}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	input := changeTestInput()
	bundle := changeTestSource(t, &input)
	input.PolicyID, input.RunID, input.AttemptID = f.policy.PolicyID, source.RunID, source.AttemptID
	version, err := native.PublishChangeVersion(ctx, f.leases[0].WorkItemID, change.ID, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source_version"}, ChangeVersionInput: input, SourceBundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	old := f.leases[0]
	if err := native.Transport().Request(ctx, http.MethodPost, native.Scope()+"/leases/"+string(old.ID)+"/release", tracker.NativeLeaseMutation{FencingToken: old.FencingToken, Reason: "released"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(ctx, "prj_runner", old.WorkItemID)
		if err != nil {
			return err
		}
		lease, err := w.Claim(ctx, leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(old.MachineID), SessionID: "review_execution", LeaseID: "review_lease", TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(ctx, runnerstore.LeaseAssignment{OrganizationID: "org_startup", LeaseID: lease.LeaseID, RunnerID: f.bindings[0].RunnerID, AssignedAt: now}); err != nil {
			return err
		}
		if err := w.BindLeasePolicy(ctx, policystore.LeasePolicy{OrganizationID: "org_startup", ProjectID: "prj_runner", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: f.policy.PolicyID, BoundAt: now}); err != nil {
			return err
		}
		f.leases[0] = tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: old.WorkItemID, MachineID: old.MachineID, SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: f.policy.PolicyID, ExpiresAt: lease.ExpiresAt}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx = api.WithMutationAuthority(t.Context(), native.Scope(), f.leases[0], nil)
	active := tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), Sequence: 1, LeaseID: f.leases[0].ID, FencingToken: f.leases[0].FencingToken, PolicyID: f.policy.PolicyID, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
	if err := native.AppendEvent(ctx, old.WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "review_started"}, Type: "run.started", SchemaVersion: 1, Data: active}); err != nil {
		t.Fatal(err)
	}
	codingSession := session("code")
	active.Sequence = 2
	active.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC(), Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: codingSession, Status: "running"}}
	if err := native.AppendEvent(ctx, old.WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "review_runtime"}, Type: "run.observed", SchemaVersion: 1, Data: active}); err != nil {
		t.Fatal(err)
	}
	makeResult := func(id int64) gate.ValidatorResult {
		return gate.ValidatorResult{VersionID: version.ID, SessionID: id, Submitted: true, Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, DiffDigest: version.Source.DiffSHA256, Score: 1, Verdict: gate.ValidatorVerdictPass}
	}
	implementation := makeResult(session("validator"))
	for _, test := range []struct {
		name   string
		change func(*gate.ValidatorResult)
	}{
		{name: "implementing_session", change: func(result *gate.ValidatorResult) { result.SessionID = sourceSession }},
		{name: "active_coding_session", change: func(result *gate.ValidatorResult) { result.SessionID = codingSession }},
		{name: "different_source", change: func(result *gate.ValidatorResult) { result.DiffDigest = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := implementation
			test.change(&invalid)
			if _, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedVersionID: version.ID, Decision: "approved", ImplementationReview: &invalid}); err == nil {
				t.Fatal("worker published a review with invalid source or session authority")
			}
		})
	}
	request := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "implementation_review"}, ExpectedVersionID: version.ID, Decision: "approved", ImplementationReview: &implementation}
	review, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, request)
	if err != nil || replay.ID != review.ID {
		t.Fatalf("review receipt replay: %+v %v", replay, err)
	}
	acceptance := makeResult(implementation.SessionID)
	if _, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "shared_session"}, Decision: "approved", Validator: &acceptance}); err == nil {
		t.Fatal("acceptance reused implementation review session")
	}
	acceptance.SessionID = session("validator")
	accepted, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "acceptance_review"}, ExpectedVersionID: version.ID, Decision: "approved", Validator: &acceptance})
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := f.client(t, 0)
	readPath := native.Scope() + "/work-items/" + string(old.WorkItemID) + "/changes/" + change.ID + "?view=bounded&section=reviews&limit=1"
	var first, second operatortool.ChangeResult
	if err := restarted.Transport().Request(ctx, http.MethodGet, readPath, nil, &first); err != nil {
		t.Fatal(err)
	}
	if first.Detail == nil || first.Read == nil || len(first.Detail.Reviews) != 1 || first.Detail.Reviews[0].ID != review.ID || first.Read.Sections["reviews"].Complete || first.Read.Sections["reviews"].NextCursor == "" {
		t.Fatalf("review page lost its bounded identity or continuation: %+v", first)
	}
	if err := restarted.Transport().Request(ctx, http.MethodGet, readPath+"&cursor="+first.Read.Sections["reviews"].NextCursor, nil, &second); err != nil {
		t.Fatal(err)
	}
	if second.Detail == nil || second.Read == nil || len(second.Detail.Reviews) != 1 || second.Detail.Reviews[0].ID != accepted.ID || !second.Read.Sections["reviews"].Complete {
		t.Fatalf("review continuation lost the accepted review: %+v", second)
	}
	detail, err := restarted.RuntimeChange(ctx, old.WorkItemID, change.ID)
	if err != nil || detail.Summary.Status != "reviewed" || len(detail.Reviews) != 2 {
		t.Fatalf("published reviews did not satisfy gates: %+v %v", detail, err)
	}
	t.Run("irrelevant historical reviews do not block current validation", func(t *testing.T) {
		content := eventstore.NewContent(f.service.runDetailStore())
		rows := make([]eventstore.Evidence, 201)
		for i := range rows {
			review := tracker.ChangeReview{ID: fmt.Sprintf("historical_review_%d", i), VersionID: version.ID, Decision: "commented", Actor: tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}, CreatedAt: now.Add(-time.Hour)}
			raw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			rows[i], err = content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: change.ID, VersionID: new(version.ID), Kind: "review", CreatedAt: review.CreatedAt}, raw)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			for _, row := range rows {
				if _, err := w.AppendEvidence(ctx, row); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		current := makeResult(session("validator"))
		if _, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "validation_after_history"}, ExpectedVersionID: version.ID, Decision: "approved", Validator: &current}); err != nil {
			t.Fatalf("unrelated historical reviews blocked current validation: %v", err)
		}
		reused := makeResult(implementation.SessionID)
		_, err := native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "session_reuse_after_history"}, ExpectedVersionID: version.ID, Decision: "approved", Validator: &reused})
		var refusal *api.APIError
		if !errors.As(err, &refusal) || refusal.Code != "invalid_request" {
			t.Fatal("historical cross-kind session reuse did not fail as an invalid review")
		}
		malformed := tracker.ChangeReview{ID: "metadata_mismatch", VersionID: version.ID, Decision: "approved", SessionMachineID: review.SessionMachineID, Actor: review.Actor, ImplementationReview: &implementation, CreatedAt: time.Now().UTC()}
		raw, err := json.Marshal(malformed)
		if err != nil {
			t.Fatal(err)
		}
		row, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: change.ID, Kind: "review"}, raw)
		if err != nil {
			t.Fatal(err)
		}
		row.ActorPrincipalID = "different-actor"
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(ctx, row); return err }); err != nil {
			t.Fatal(err)
		}
		fresh := makeResult(session("validator"))
		_, err = native.ReviewChange(ctx, old.WorkItemID, change.ID, version.ID, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "validation_after_metadata_mismatch"}, ExpectedVersionID: version.ID, Decision: "approved", Validator: &fresh})
		if !errors.As(err, &refusal) || refusal.Status != http.StatusInternalServerError {
			t.Fatal("current validation trusted a review whose actor differs from its canonical row")
		}
		malformed.ID = "ambiguous_history"
		malformed.Validator = &current
		raw, err = json.Marshal(malformed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: "org_startup", ChangeID: change.ID, Kind: "review"}, raw); !errors.Is(err, eventstore.ErrInvalidRecord) {
			t.Fatal("current evidence preparation accepted combined machine results")
		}
		ref, err := f.service.runDetailStore().StoreDetail(ctx, "org_startup", raw)
		if err != nil {
			t.Fatal(err)
		}
		row.ActorPrincipalID = malformed.Actor.PrincipalID
		row.ReviewKind, row.ReviewLanding, row.ReviewSessionKnown, row.ReviewSessionID, row.ReviewSessionMachine = "", false, false, 0, ""
		row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
		if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_startup", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(ctx, row); return err }); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.config.TenantRepository.(*postgres.Repository).BackfillChangeRecoveryProjections(ctx, "org_startup", content, 1); !errors.Is(err, eventstore.ErrInvalidRecord) {
			t.Fatal("historical combined machine results hid a session during canonical backfill")
		}
	})
	active.Sequence = 3
	active.Outcome = "succeeded"
	active.Runtime.Phase = "completed"
	active.Runtime.HeartbeatAt = time.Now().UTC()
	if err := native.AppendEvent(ctx, old.WorkItemID, tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "review_finished"}, Type: "run.finished", SchemaVersion: 1, Data: active}); err != nil {
		t.Fatal(err)
	}
	var completed tracker.NativeAttempt
	if err := restarted.Transport().Request(ctx, http.MethodGet, native.Scope()+"/work-items/"+string(old.WorkItemID)+"/attempts/"+active.AttemptID, nil, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status != "succeeded" {
		t.Fatalf("review attempt did not complete: %+v", completed)
	}
}
