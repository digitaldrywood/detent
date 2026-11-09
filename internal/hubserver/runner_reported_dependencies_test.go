package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type rejectedRunnerDependencyParticipant struct {
	NativeRunEventParticipant
	err error
}

type runnerDependencyBarrierContextKey struct{}

type runnerDependencyPayloadBarrier struct {
	attachment.DetailStore
	mu      sync.Mutex
	attempt string
	ready   chan struct{}
	release chan struct{}
}

func (s *runnerDependencyPayloadBarrier) configure(attempt string, ready, release chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt, s.ready, s.release = attempt, ready, release
}

func (s *runnerDependencyPayloadBarrier) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.mu.Lock()
	attempt, ready, release := s.attempt, s.ready, s.release
	s.mu.Unlock()
	var data tracker.NativeRunData
	pause, enabled := ctx.Value(runnerDependencyBarrierContextKey{}).(*sync.Once)
	if enabled && attempt != "" && json.Unmarshal(raw, &data) == nil && data.AttemptID == attempt && data.Outcome == "succeeded" {
		var waitErr error
		pause.Do(func() {
			select {
			case ready <- struct{}{}:
			case <-ctx.Done():
				waitErr = ctx.Err()
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				waitErr = ctx.Err()
			}
		})
		if waitErr != nil {
			return attachment.ArchiveReference{}, waitErr
		}
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}

func appendPreparedRunnerDependencyReports(t *testing.T, service *Service, scope nativeScope, item string, event tracker.NativeRunEvent, barrier *runnerDependencyPayloadBarrier, count int, interleave func(), wantErr error) {
	t.Helper()
	ready, released := make(chan struct{}, 2), make(chan struct{})
	barrier.configure(event.Data.AttemptID, ready, released)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	var release sync.Once
	resume := func() { release.Do(func() { close(released) }) }
	results := make(chan error, 2)
	t.Cleanup(func() {
		cancel()
		resume()
		workers.Wait()
		barrier.configure("", nil, nil)
	})
	for range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			workerContext := context.WithValue(ctx, runnerDependencyBarrierContextKey{}, &sync.Once{})
			_, err := service.appendNativeRunEventTenant(workerContext, scope, item, event)
			results <- err
		}()
	}
	for range count {
		select {
		case <-ready:
		case err := <-results:
			t.Fatalf("completion returned before both reports prepared: %v", err)
		}
	}
	if interleave != nil {
		interleave()
	}
	resume()
	for range count {
		err := <-results
		if !errors.Is(err, wantErr) {
			t.Fatalf("prepared completion result=%v, want %v", err, wantErr)
		}
	}
	workers.Wait()
	barrier.configure("", nil, nil)
	cancel()
}

func (p rejectedRunnerDependencyParticipant) ApplyNativeRunEvent(ctx context.Context, w tenantstore.Writer, authority NativeRunEventAuthority) error {
	if err := p.NativeRunEventParticipant.ApplyNativeRunEvent(ctx, w, authority); err != nil {
		return err
	}
	return p.err
}

func TestRunnerCompletionRecordsDependencyEdgesAtomically(t *testing.T) {
	barrier := &runnerDependencyPayloadBarrier{}
	f := newRunnerPGFixtureWithDetails(t, func(store attachment.DetailStore) attachment.DetailStore {
		barrier.DetailStore = store
		return barrier
	})
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), "prj_runner")
		if err != nil {
			return err
		}
		project.Settings.Repository = "example-org/example-project"
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, lease := range f.leases {
		var plan workItemRecordPlan
		var version eventstore.CollaborationVersion
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			plan.project, err = r.ProjectRecord(t.Context(), "prj_runner")
			if err != nil {
				return err
			}
			plan.current, err = r.WorkItem(t.Context(), "prj_runner", lease.WorkItemID)
			if err != nil {
				return err
			}
			version, err = r.CollaborationVersion(t.Context(), string(lease.WorkItemID), plan.current.Revision)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var err error
		plan.issue, err = f.service.hydrateCanonicalWorkItemRecord(t.Context(), plan.current, version)
		if err != nil {
			t.Fatal(err)
		}
		plan.issue.CreatedAt, plan.issue.UpdatedAt = plan.current.CreatedAt, plan.current.UpdatedAt
		document, err := searchstore.NewContent(f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, ID: "issue:" + string(lease.WorkItemID), SourceID: string(lease.WorkItemID), Kind: "issue", Scope: "issue", Revision: plan.current.Revision, UpdatedAt: plan.current.UpdatedAt}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			if err := writeClaimFactsAndQueue(t.Context(), w, plan, nil); err != nil {
				return err
			}
			return w.PutSearch(t.Context(), document, 0)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, lease := range f.leases {
		t.Run(string(lease.WorkItemID), func(t *testing.T) {
			base := "/api/v2/organizations/org_startup/projects/prj_runner/work-items"
			var blockers []tracker.NativeIssue
			for targetIndex := range 2 {
				now := time.Now().UTC()
				blocker := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_blocker_%d_%d", index, targetIndex)), Number: index*2 + targetIndex + 3, Revision: 1}, Title: "Example prerequisite", State: "Todo", CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(blocker)
				if err != nil {
					t.Fatal(err)
				}
				version, err := eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: blocker.WorkItemID, RecordID: string(blocker.WorkItemID), Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_startup", ProjectID: "prj_runner", NativeID: blocker.WorkItemID, Number: int64(blocker.Number), Title: blocker.Title, WorkflowStateName: &blocker.State, CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					_, err := w.CreateCollaborationVersion(t.Context(), version)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				blockers = append(blockers, blocker)
			}
			data := f.data[index]
			data.Sequence, data.Outcome = 2, "succeeded"
			reference := fmt.Sprintf("example-org/example-project#%d", blockers[0].Number)
			if index == 1 {
				reference = fmt.Sprintf("prj_runner#%d", blockers[0].Number)
			}
			data.CompletionBody = fmt.Sprintf("```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '%s'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\n  - ref: '#%d'\n    reason: second prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: null\n```", reference, blockers[1].Number)
			signal, reported := workpad.SignalFromComment(data.CompletionBody, "", "prj_runner")
			if !reported || signal == nil || signal.Invalid != nil {
				t.Fatal("invalid dependency report fixture")
			}
			data.Disposition = &tracker.NativeDisposition{Status: signal.Status, Blockers: true, BlockerEvidence: signal.Blockers}
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "dependency_" + data.AttemptID}, Type: "run.finished", SchemaVersion: 1, Data: data}
			path := base + "/" + string(lease.WorkItemID) + "/events"
			credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), "org_startup", apikey.HashToken(f.tokens[index]), "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: "org_startup", project: "prj_runner", credential: credential}
			rejected := errors.New("example dependency participant refusal")
			original := f.service.config.NativeRunEvents
			t.Cleanup(func() { f.service.config.NativeRunEvents = original })
			f.service.config.NativeRunEvents = rejectedRunnerDependencyParticipant{NativeRunEventParticipant: original, err: rejected}
			_, err = f.service.appendNativeRunEventTenant(t.Context(), scope, string(lease.WorkItemID), event)
			f.service.config.NativeRunEvents = original
			if !errors.Is(err, rejected) {
				t.Fatalf("post-dependency participant rejection: %v", err)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(t.Context(), "prj_runner", lease.WorkItemID)
				if err != nil {
					return err
				}
				attempt, err := r.Attempt(t.Context(), data.AttemptID)
				if err != nil {
					return err
				}
				page, err := r.Relationships(t.Context(), "prj_runner", lease.WorkItemID, corestore.RelationshipCursor{}, 10)
				if err == nil && (len(page.Items) != 0 || item.Revision != 1 || attempt.Sequence != 1 || attempt.Status != "running") {
					t.Errorf("rejected completion leaked edges/revision/attempt: edges=%d revision=%d sequence=%d status=%s", len(page.Items), item.Revision, attempt.Sequence, attempt.Status)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			appendPreparedRunnerDependencyReports(t, f.service, scope, string(lease.WorkItemID), event, barrier, 1, func() {
				var source corestore.WorkItem
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					source, err = r.WorkItem(t.Context(), "prj_runner", lease.WorkItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				comment := tracker.NativeComment{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, ID: newNativeID("cmt"), Revision: 1, Sequence: source.EventSequence + 1, Body: "Example feedback", Actor: scope.actor(), CreatedAt: now, UpdatedAt: now}
				fields, err := f.service.prepareWorkItemFields(t.Context(), "org_startup", workItemRecordPlan{comment: comment})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(comment)
				if err != nil {
					t.Fatal(err)
				}
				content := eventstore.NewContent(f.service.runDetailStore())
				version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, RecordID: comment.ID, Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: "org_startup", ProjectID: "prj_runner", AggregateID: lease.WorkItemID, Type: "comment.created", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: 1, CommentID: comment.ID}})
				if err != nil {
					t.Fatal(err)
				}
				history, err := content.PrepareEvent(t.Context(), "org_startup", raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: "org_startup", ProjectID: "prj_runner", WorkItemID: lease.WorkItemID, ID: comment.ID, Sequence: comment.Sequence, BodyObjectKey: fields.Body, ActorKind: comment.Actor.Kind, ActorPrincipalID: comment.Actor.PrincipalID, CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					_, err := w.AppendEvent(t.Context(), history)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}, corestore.ErrSerializationFailure)
			appendPreparedRunnerDependencyReports(t, f.service, scope, string(lease.WorkItemID), event, barrier, 2, nil, nil)
			var revision int64
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(t.Context(), "prj_runner", lease.WorkItemID)
				if err != nil {
					return err
				}
				revision = item.Revision
				page, err := r.Relationships(t.Context(), "prj_runner", lease.WorkItemID, corestore.RelationshipCursor{}, 10)
				if err != nil {
					return err
				}
				if len(page.Items) != 2 || item.Revision != 3 {
					t.Errorf("completion dependency edges=%+v revision=%d, want both prerequisites at revision3", page.Items, item.Revision)
				}
				for _, blocker := range blockers {
					found := false
					for _, edge := range page.Items {
						found = found || edge.RelatedWorkItemID == blocker.WorkItemID && edge.Type == "dependency"
					}
					if !found {
						t.Errorf("completion lost prerequisite %s", blocker.WorkItemID)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			native, ctx := f.client(t, index)
			var persisted tracker.NativeAttempt
			if err := native.Transport().Request(ctx, http.MethodGet, native.Scope()+"/work-items/"+string(lease.WorkItemID)+"/attempts/"+data.AttemptID, nil, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.CompletionBody != data.CompletionBody || persisted.Disposition == nil || len(persisted.Disposition.BlockerEvidence) != 2 || persisted.Disposition.BlockerEvidence[0].Ref != fmt.Sprintf("prj_runner#%d", blockers[0].Number) || data.Disposition.BlockerEvidence[0].Ref != reference {
				t.Fatal("dependency normalization changed the original report/input or lost durable native identity")
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], event), http.StatusOK)
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				item, err := r.WorkItem(t.Context(), "prj_runner", lease.WorkItemID)
				if err == nil && item.Revision != revision {
					t.Errorf("replayed completion changed revision: %d -> %d", revision, item.Revision)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
