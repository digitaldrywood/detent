package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCollaborationPayloadInlineBytes(t *testing.T) {
	t.Parallel()
	store := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{Content: (&changeObjectFixture{}).call, RunDetails: store, Conversation: &ConversationConfig{Enabled: true}, now: func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }})
	repository := f.service.database.postgres
	admin := usagePostgresAdmin(t, f.service)
	content := eventstore.NewContent(store)
	var inlineColumns int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='collaboration_events' AND (data_type IN ('json','jsonb') OR column_name IN ('data_json','payload_json'))").Scan(&inlineColumns); err != nil || inlineColumns != 0 {
		t.Fatalf("inline payload columns=%d error=%v", inlineColumns, err)
	}
	for _, kind := range []string{"run.started", "run.checkpointed", "run.observed", "run.finished", "scheduler.decision"} {
		t.Run(kind, func(t *testing.T) {
			var sizes []int
			payloadSizes := []int{16, 256 << 10}
			if kind == "scheduler.decision" {
				payloadSizes = []int{0}
			}
			for _, size := range payloadSizes {
				issue := f.create(t, fmt.Sprintf("object payload %s %d", kind, size))
				scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
				scope.Credential = hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}
				ctx := t.Context()
				data := tracker.CollaborationData{Revision: issue.Revision}
				if kind == "scheduler.decision" {
					data.Decision = &tracker.NativeSchedulerDecision{RunnerID: "runner", Source: "native_claim", Outcome: "skipped", Reason: "capacity", At: f.service.config.now(), WorkItemRevision: issue.Revision}
					data.Change = &tracker.NativeChangeReference{ChangeID: "change", VersionID: "version", HeadSHA: "head"}
					data.ReasonDetail = "retain scheduler audit context"
				} else {
					run := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: "lease", PolicyID: "policy", FencingToken: 1, Sequence: 1, CompletionBody: strings.Repeat("x", size)}
					data.Run = &run
				}
				actor := scope.Actor()
				actor.ViaKey, actor.DisplayName, actor.Agent = "example_key", "Example operator", "Example assistant"
				raw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: kind, SchemaVersion: 1, Actor: actor, RecordedAt: f.service.config.now(), Data: data})
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := content.PrepareEvent(ctx, scope.Organization, raw)
				if err != nil {
					t.Fatal(err)
				}
				var event eventstore.Event
				if err := repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					event, err = w.AppendEvent(ctx, prepared)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var inline int
				if err := admin.QueryRowContext(ctx, "SELECT pg_column_size(e) FROM collaboration_events e WHERE organization_id=$1 AND id=$2", scope.Organization, event.ID).Scan(&inline); err != nil {
					t.Fatal(err)
				}
				sizes = append(sizes, inline)
				var page tracker.Page[tracker.CollaborationEvent]
				for _, view := range []string{"", "timeline"} {
					t.Run(fmt.Sprintf("%d/view=%s", size, view), func(t *testing.T) {
						original := f.service.config.Content
						defer func() { f.service.config.Content = original }()
						reads := 0
						checkConnections := func(context.Context) error {
							if acquired := repository.Stats().AcquiredConns(); acquired != 0 {
								t.Errorf("object I/O holds %d database connections", acquired)
								return errors.New("object I/O holds database connections")
							}
							return nil
						}
						originalDetails := f.service.config.RunDetails
						f.service.config.RunDetails = collaborationReadProbe{DetailStore: store, beforeRead: func(ctx context.Context, ref attachment.ArchiveReference) error {
							if ref.Key == event.PayloadObjectKey {
								reads++
							}
							return checkConnections(ctx)
						}}
						defer func() { f.service.config.RunDetails = originalDetails }()
						f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
							if err := checkConnections(ctx); err != nil {
								return nil, err
							}
							return original(ctx, request)
						}
						result, err := f.service.readHistory(f.service.freshRunDetailContext(t.Context()), scope, string(issue.WorkItemID), url.Values{"limit": {"100"}, "view": {view}})
						if err != nil {
							t.Fatal(err)
						}
						if view == "" {
							page = result
						}
						if len(result.Items) != 2 {
							t.Fatalf("history items=%d, want 2", len(result.Items))
						}
						if result.Items[1].Actor != actor {
							t.Fatalf("history actor=%+v want %+v", result.Items[1].Actor, actor)
						}
						suppressed := view == "timeline" && strings.HasPrefix(kind, "run.")
						if suppressed {
							if reads != 0 || !reflect.DeepEqual(result.Items[1].Data, tracker.CollaborationData{}) {
								t.Fatal("timeline hydrated suppressed run payload")
							}
						} else if reads == 0 {
							t.Fatal("history did not hydrate object payload")
						}
					})
				}
				if kind == "run.started" && size == 16 {
					for _, field := range []string{"key", "name", "agent"} {
						t.Run("actor projection "+field, func(t *testing.T) {
							changed := event
							switch field {
							case "key":
								changed.ActorViaKey = "other_key"
							case "name":
								changed.ActorDisplayName = "Other operator"
							case "agent":
								changed.ActorAgent = "Other assistant"
							}
							if _, err := content.Event(t.Context(), changed); !errors.Is(err, artifact.ErrIntegrity) {
								t.Fatalf("actor projection mismatch error=%v", err)
							}
						})
					}
					t.Run("historical actor recovery and replay", func(t *testing.T) {
						tx, err := admin.BeginTx(t.Context(), nil)
						if err != nil {
							t.Fatal(err)
						}
						defer tx.Rollback()
						if _, err := tx.ExecContext(t.Context(), "SET LOCAL session_replication_role=replica"); err != nil {
							t.Fatal(err)
						}
						if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET actor_envelope_known=false,actor_via_key='',actor_display_name='',actor_agent='' WHERE organization_id=$1 AND id=$2", scope.Organization, event.ID); err != nil {
							t.Fatal(err)
						}
						if err := tx.Commit(); err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
							replayed, err := w.AppendEvent(t.Context(), prepared)
							if err == nil && (replayed.ID != event.ID || replayed.Sequence != event.Sequence || replayed.ActorEnvelopeKnown) {
								return errors.New("historical replay changed the event")
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
						original := f.service.config.RunDetails
						read := false
						f.service.config.RunDetails = &changeReadInterceptionStore{DetailStore: original, key: event.PayloadObjectKey, afterRead: func(context.Context) error {
							read = true
							if repository.Stats().AcquiredConns() != 0 {
								return errors.New("historical actor I/O holds database connections")
							}
							return nil
						}}
						defer func() { f.service.config.RunDetails = original }()
						recovered, err := f.service.readHistory(f.service.freshRunDetailContext(t.Context()), scope, string(issue.WorkItemID), url.Values{"view": {"timeline"}})
						if err != nil || !read || len(recovered.Items) != 2 {
							t.Fatalf("historical actor read=%v rows=%d error=%v", read, len(recovered.Items), err)
						}
						if recovered.Items[1].Actor != actor || !reflect.DeepEqual(recovered.Items[1].Data, tracker.CollaborationData{}) {
							t.Fatalf("historical timeline event=%+v", recovered.Items[1])
						}
					})
				}
				if len(page.Items) != 2 {
					t.Fatalf("history items=%d, want 2", len(page.Items))
				}
				if kind == "scheduler.decision" {
					created, err := f.service.commandCreateConversation(ctx, scope, conversationCreateRequest{Key: "payload-conversation", Title: "Example", SubjectWorkItemID: string(issue.WorkItemID)})
					if err != nil {
						t.Fatal(err)
					}
					var conversation conversationCreatedResponse
					if err := json.Unmarshal(created, &conversation); err != nil {
						t.Fatal(err)
					}
					for _, lightweight := range []bool{false, true} {
						original := f.service.config.Content
						reads := 0
						store.beforeRead = func(context.Context) error {
							reads++
							if acquired := repository.Stats().AcquiredConns(); acquired != 0 {
								return errors.New("object I/O holds database connections")
							}
							return nil
						}
						f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
							reads++
							if acquired := repository.Stats().AcquiredConns(); acquired != 0 {
								t.Errorf("object I/O holds %d database connections", acquired)
								return nil, errors.New("object I/O holds database connections")
							}
							return original(ctx, request)
						}
						ctx := context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, lightweight)
						runtime, err := f.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
						if err == nil {
							_, err = f.service.readConversationSnapshotFor(f.service.runDetailContext(ctx), scope, conversation.Conversation.ID, "", true)
						}
						f.service.config.Content = original
						store.beforeRead = nil
						if err != nil {
							t.Fatal(err)
						}
						if !lightweight && reads == 0 {
							t.Fatal("runtime did not hydrate scheduler audit payload")
						}
						want := page.Items[1].Data
						if lightweight {
							want.ReasonDetail = ""
						}
						for name, event := range map[string]*tracker.CollaborationEvent{"latest decision": runtime.LatestDecision, "candidate exclusion": runtime.NativeCandidateExclusion} {
							if event == nil || !reflect.DeepEqual(event.Data, want) {
								t.Errorf("%s payload differs from history (lightweight=%v): got %+v, want %+v", name, lightweight, event, want)
							}
						}
					}
				}
				if size < 1024 || kind == "scheduler.decision" {
					if !reflect.DeepEqual(page.Items[1].Data, data) {
						t.Fatal("history differs from original payload")
					}
				} else if page.Items[1].DataOmission == nil {
					t.Fatal("large history payload was not bounded")
				}
				decoded, err := content.Event(ctx, event)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded.Data, data) {
					t.Fatal("object payload differs from input")
				}
				var queued int
				if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM storage_conversion_progress WHERE organization_id=$1 AND family='runs' AND aggregate_id=$2`, scope.Organization, issue.WorkItemID).Scan(&queued); err != nil || queued != 0 {
					t.Fatalf("new event queued for conversion: %d, %v", queued, err)
				}
			}
			if len(sizes) > 1 && sizes[0] != sizes[1] {
				t.Fatalf("inline payload grows: %v", sizes)
			}
		})
	}
}

func TestNativeRuntimeLegacyExclusionProjection(t *testing.T) {
	t.Parallel()
	details := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: details})
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: hubapi.APICredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: hubapi.APIScopeAdmin}}
	content := eventstore.NewContent(f.service.runDetailStore())
	unrelated := make([]eventstore.Event, 128)
	for index := range unrelated {
		item := tracker.NativeWorkItemID(fmt.Sprintf("wi_unrelated_%d", index))
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("evt_unrelated_%d", index), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "ready", Reason: "available"}}})
		if err != nil {
			t.Fatal(err)
		}
		unrelated[index], err = content.PrepareEvent(t.Context(), scope.Organization, raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		for index, event := range unrelated {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: scope.Organization, ProjectID: scope.Project, NativeID: event.WorkItemID, Number: int64(10000 + index), Title: "Example unrelated item"}); err != nil {
				return err
			}
			if _, err := w.AppendEvent(t.Context(), event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"first scheduler history", "second scheduler history"} {
		issue := f.create(t, title)
		records := make([]eventstore.Event, 0, 131)
		for i := range 131 {
			decision := tracker.NativeSchedulerDecision{Outcome: "ready", Reason: "available"}
			if i < 2 {
				decision.Outcome = "skipped"
				decision.Reason = fmt.Sprintf("historical refusal %d", i)
			}
			raw, err := json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: issue.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: f.service.config.now(), Data: tracker.CollaborationData{Decision: &decision}})
			if err != nil {
				t.Fatal(err)
			}
			record, err := content.PrepareEvent(t.Context(), scope.Organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			if i == 1 || i == 130 {
				record.PayloadFormatVersion = 1
			}
			records = append(records, record)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, record := range records {
				if _, err := w.AppendEvent(t.Context(), record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		admin := usagePostgresAdmin(t, f.service)
		var headQuery string
		for _, tc := range []struct {
			name, outcome string
			incomplete    bool
			want          error
		}{
			{"current", "ready", false, nil},
			{"historical outcome unknown", "", true, nil},
			{"typed outcome mismatch", "claimed", false, artifact.ErrIntegrity},
			{"restored", "ready", false, nil},
		} {
			t.Run(title+"/"+tc.name, func(t *testing.T) {
				tx, err := admin.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(t.Context(), "DELETE FROM scheduler_event_heads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND head_kind='unprojected'", scope.Organization, scope.Project, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE scheduler_event_heads SET scheduler_outcome=$1 WHERE organization_id=$2 AND project_id=$3 AND work_item_id=$4 AND head_kind='latest'", tc.outcome, scope.Organization, scope.Project, issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if tc.incomplete {
					if _, err := tx.ExecContext(t.Context(), "INSERT INTO scheduler_event_heads(head_kind, organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, project_sequence, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state, scheduler_outcome) SELECT 'unprojected', organization_id, project_id, work_item_id, id, sequence, type, schema_version, actor_kind, actor_principal_id, event_revision, change_id, version_id, head_sha, run_id, attempt_id, from_state, to_state, recorded_at, payload_object_key, payload_sha256, payload_bytes, payload_content_bytes, payload_format_version, landing_waiting, landing_change_id, landing_version_id, landing_head_sha, project_sequence, push_known, push_change_id, push_version_id, push_head_sha, push_external_effect, push_effect_state, scheduler_outcome FROM scheduler_event_heads WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND head_kind='latest'", scope.Organization, scope.Project, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				for _, lightweight := range []bool{false, true} {
					ctx := postgres.WithTransactionTiming(context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, lightweight), nil, func(_ time.Time, query string) {
						if strings.Contains(query, "-- name: SchedulerEventHead") {
							headQuery = query
						}
					})
					evidence, err := f.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
					if !errors.Is(err, tc.want) {
						t.Fatalf("runtime error=%v want=%v", err, tc.want)
					}
					if err != nil {
						continue
					}
					if evidence.LatestDecision == nil || evidence.LatestDecision.Data.Decision.Outcome != "ready" {
						t.Fatal("latest decision lost")
					}
					if slices.Contains(evidence.Unavailable, "scheduler_history_projection") != tc.incomplete {
						t.Fatalf("projection availability=%v", evidence.Unavailable)
					}
					if tc.incomplete {
						if evidence.NativeCandidateExclusion != nil {
							t.Fatal("unknown history produced an authoritative exclusion")
						}
						continue
					}
					if event := evidence.NativeCandidateExclusion; event == nil || event.Data.Decision == nil || event.Data.Decision.Reason != "historical refusal 1" {
						t.Fatalf("latest historical exclusion was lost (lightweight=%v): %+v", lightweight, event)
					}
				}
			})
		}
		t.Run(title+"/converted selected heads", func(t *testing.T) {
			for _, kind := range []string{"latest", "excluded"} {
				t.Run(kind, func(t *testing.T) {
					var heads eventstore.SchedulerDecisionRows
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						heads, err = r.(eventstore.SchedulerDecisionReader).SchedulerDecisions(t.Context(), scope.Project, issue.WorkItemID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					selected := heads.Latest
					if kind == "excluded" {
						selected = heads.Exclusion
					}
					if selected == nil {
						t.Fatal("scheduler head is missing before conversion")
					}
					event, err := content.Event(t.Context(), *selected)
					if err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(event)
					if err != nil {
						t.Fatal(err)
					}
					target, err := details.StoreDetail(t.Context(), string(scope.Organization), append(raw, '\n'))
					if err != nil {
						t.Fatal(err)
					}
					source := eventstore.Reference(selected.PayloadObjectKey, selected.PayloadSHA256, selected.PayloadBytes, selected.PayloadContentBytes)
					if target.Key == source.Key {
						t.Fatal("conversion did not replace the stored object")
					}

					candidate := storageconversion.Candidate{Scope: storageconversion.Scope{Organization: scope.Organization, Project: scope.Project, Family: storageconversion.Runs, AggregateID: string(issue.WorkItemID)}, Position: storageconversion.Position{ID: selected.ID, Sequence: selected.Sequence}, FormatVersion: 1, Source: source}
					converted, err := f.service.config.TenantRepository.(storageconversion.Repository).CommitConversion(t.Context(), candidate, target)
					if err != nil || !converted {
						t.Fatalf("convert selected scheduler event: committed=%t error=%v", converted, err)
					}
					details.mu.Lock()
					delete(details.data, source.Key)
					details.mu.Unlock()
					if _, err := details.ReadDetail(t.Context(), string(scope.Organization), source); !errors.Is(err, artifact.ErrMissing) {
						t.Fatalf("old scheduler object remains available: %v", err)
					}
					for _, lightweight := range []bool{false, true} {
						ctx := context.WithValue(t.Context(), hubcontent.ExplanationReadKey{}, lightweight)
						evidence, err := f.service.readNativeRuntime(ctx, scope, string(issue.WorkItemID), "")
						if err != nil {
							t.Fatalf("converted scheduler evidence (explanation=%t): %v", lightweight, err)
						}
						if evidence.LatestDecision == nil || evidence.LatestDecision.ID != heads.Latest.ID || evidence.NativeCandidateExclusion == nil || evidence.NativeCandidateExclusion.ID != heads.Exclusion.ID || evidence.NativeCandidateExclusion.Data.Decision.Reason != "historical refusal 1" {
							t.Fatal("conversion changed selected scheduler decisions")
						}
					}
				})
			}
		})
		t.Run(title+"/bounded history heads", func(t *testing.T) {
			if headQuery == "" {
				t.Fatal("scheduler history query was not observed")
			}
			tx, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for _, statement := range []string{"SET LOCAL ROLE detent_runtime_v2", "SET LOCAL plan_cache_mode=force_generic_plan"} {
				if _, err := tx.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", scope.Organization); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "PREPARE scheduler_history_heads(text,text,text) AS "+headQuery); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := tx.ExecContext(t.Context(), "DEALLOCATE scheduler_history_heads"); err != nil {
					t.Error(err)
				}
			}()
			literal := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
			query := "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE scheduler_history_heads(" + literal(string(scope.Organization)) + "," + literal(string(scope.Project)) + "," + literal(string(issue.WorkItemID)) + ")"
			var raw []byte
			if err := tx.QueryRowContext(t.Context(), query).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan schedulerHistoryPlan }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode scheduler plan: %v", err)
			}
			var visits float64
			var inspect func(schedulerHistoryPlan)
			inspect = func(plan schedulerHistoryPlan) {
				if plan.Relation == "collaboration_events" || plan.Relation == "scheduler_event_heads" {
					visits += (plan.Rows + plan.Removed + plan.Rechecked) * plan.Loops
				}
				for _, child := range plan.Plans {
					inspect(child)
				}
			}
			inspect(plans[0].Plan)
			if visits > 12 {
				t.Errorf("scheduler evidence visited %.0f history rows, want at most12; plan=%s", visits, raw)
			}
			t.Logf("scheduler evidence row visits=%.0f", visits)
		})

	}
}

type schedulerArchiveFailureStore struct {
	attachment.DetailStore
	failure string
}

func (s schedulerArchiveFailureStore) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if s.failure == "upload rejected" {
		return attachment.ArchiveReference{}, artifact.ErrStorage
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}

func (s schedulerArchiveFailureStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.failure == "verification failed" {
		return []byte("invalid archive"), nil
	}
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestSchedulerPayloadRequiresArchive(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "scheduler archive required")
	runner := prepareRunner(t, f, runnerauth.Claim, runnerauth.Heartbeat)
	runner.enroll(t)
	reportRunnerCapacity(t, f.service, f.base, runner.redemption.Credential, runner.binding.MachineID, runner.redemption.DisplayName, runner.redemption.BackendIsolation, runner.redemption.Capacity)
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), scope.Organization, apikey.HashToken(runner.redemption.Credential), "", "")
	if err != nil {
		t.Fatalf("runner authentication status=%d error=%v", status, err)
	}
	scope.Credential = credential
	repository := f.service.config.TenantRepository
	var before []eventstore.Event
	var itemID int64
	if err := repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(t.Context(), scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		itemID = item.ID
		before, err = r.Events(t.Context(), scope.Project, issue.WorkItemID, 0, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	unchanged := func(t *testing.T) {
		t.Helper()
		if err := repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.ActiveLeaseForIssue(t.Context(), itemID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("failed archive published a lease: %v", err)
			}
			events, err := r.Events(t.Context(), scope.Project, issue.WorkItemID, 0, 100)
			if err == nil && !reflect.DeepEqual(events, before) {
				t.Fatal("failed archive changed event history")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("write without prepared archive", func(t *testing.T) {
		err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.AppendEvent(t.Context(), eventstore.Event{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: issue.WorkItemID, ID: hubapi.NewID("evt"), Type: "scheduler.decision", SchemaVersion: 1, ActorKind: "machine", ActorPrincipalID: scope.Credential.ID, SchedulerOutcome: "skipped"})
			return err
		})
		if !errors.Is(err, eventstore.ErrInvalidRecord) {
			t.Fatalf("unprepared event error=%v", err)
		}
		unchanged(t)
	})
	request := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "scheduler-session", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	for _, failure := range []string{"missing storage", "upload rejected", "verification failed"} {
		t.Run(failure, func(t *testing.T) {
			now := f.service.config.now()
			plan, err := f.service.readNativeClaimPlan(t.Context(), scope, request, now)
			if err != nil {
				t.Fatal(err)
			}
			original := f.service.config.RunDetails
			defer func() { f.service.config.RunDetails = original }()
			if failure == "missing storage" {
				f.service.config.RunDetails = nil
			} else {
				f.service.config.RunDetails = schedulerArchiveFailureStore{DetailStore: original, failure: failure}
			}
			_, err = f.service.claimNativeIssueTenantWithPlan(t.Context(), scope, request, 90*time.Second, now, plan, nil)
			want := artifact.ErrStorage
			status := http.StatusServiceUnavailable
			if failure == "verification failed" {
				want = artifact.ErrIntegrity
				status = http.StatusInternalServerError
			}
			if !errors.Is(err, want) {
				t.Fatalf("archive preparation error=%v want=%v", err, want)
			}
			unchanged(t)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, request)
			requireNativeStatus(t, response, status)
			unchanged(t)
		})
	}
}

type schedulerHistoryPlan struct {
	Relation  string  `json:"Relation Name"`
	Rows      float64 `json:"Actual Rows"`
	Removed   float64 `json:"Rows Removed by Filter"`
	Rechecked float64 `json:"Rows Removed by Index Recheck"`
	Loops     float64 `json:"Actual Loops"`
	Plans     []schedulerHistoryPlan
}

type collaborationReadProbe struct {
	attachment.DetailStore
	beforeRead func(context.Context, attachment.ArchiveReference) error
}

func (s collaborationReadProbe) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := s.beforeRead(ctx, ref); err != nil {
		return nil, err
	}
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}
