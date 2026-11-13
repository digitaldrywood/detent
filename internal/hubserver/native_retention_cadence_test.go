package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRetentionWebhookMaintenanceCadence(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.Add(-31 * 24 * time.Hour).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	f := newDefaultNativeFixture(t, Config{now: now, retentionTimeout: frozenRetentionTimeout, WebhookMaintenanceInterval: 10 * time.Millisecond, runMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	s := f.service
	issue := f.create(t, "closed issue")
	response := performHubAPIRequest(t, s, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", testHubAdminToken, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close-cadence-issue"}, ExpectedRevision: issue.Revision, State: "Done", Reason: "user_requested"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &issue)
	if issue.State != "Done" {
		t.Fatalf("retention fixture did not become terminal: %+v", issue)
	}
	if err := s.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		if row.TerminalEnteredAt == nil || !row.TerminalEnteredAt.Equal(start.Add(-31*24*time.Hour)) || row.Archived {
			return fmt.Errorf("terminal retention fixture facts changed: %+v", row)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock.Store(start.UnixNano())
	metrics := s.config.TenantRepository.(metricsstore.Repository)
	s.requestMetrics = newTenantTestMetrics(metrics, f.project.OrganizationID)
	content := eventstore.NewContent(s.runDetailStore())
	admin := usagePostgresAdmin(t, s)
	insertCommand := func(t *testing.T, organization tracker.OrganizationID, key string, at time.Time) {
		t.Helper()
		created := at.Add(-25 * time.Hour)
		row, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: organization, ActorID: "test_hub_admin", Operation: "POST /example", CommandKey: key, RequestHash: artifact.Digest([]byte(key)), CreatedAt: created, ResponseReplayUntilNs: created.Add(receiptstore.ReplayRetention).UnixNano()}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.PutReceipt(t.Context(), row); return err }); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 201 {
		insertCommand(t, f.project.OrganizationID, fmt.Sprintf("historical-%d", i), start)
	}
	foreign := tracker.OrganizationID("org_first_second")
	insertCommand(t, foreign, "foreign-historical", start)
	readCommands := func(organization tracker.OrganizationID) (int, error) {
		var count int
		err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_command_states WHERE organization_id=$1 AND operation='POST /example' AND NOT response_expired`, organization).Scan(&count)
		return count, err
	}
	metricTick := func(t *testing.T, key string, at time.Time) func() error {
		t.Helper()
		old := start.Add(-15 * 24 * time.Hour)
		route := "/cadence/" + key
		if err := metrics.MergeRequestMetrics(t.Context(), f.project.OrganizationID, at, []metricsstore.Metric{{At: old, Route: route, CallerKind: "browser", CallerID: "test_hub_admin", StatusClass: 2, Latency: metricsstore.Latency{Count: 1, Sum: 1, Max: 1, Histogram: [64]int64{1}}}}); err != nil {
			t.Fatal(err)
		}
		return func() error {
			rows, err := metrics.RequestMetrics(t.Context(), f.project.OrganizationID, metricsstore.Page{Grain: metricsstore.Minute, From: old, To: old.Add(time.Minute), Limit: 200})
			if err != nil {
				return err
			}
			if len(rows) == 200 {
				return fmt.Errorf("cadence fixture metric population exceeded bounded page")
			}
			for _, row := range rows {
				if row.Route == route {
					return fmt.Errorf("maintenance has not completed its metric-retention tick")
				}
			}
			return nil
		}
	}
	warmup := metricTick(t, "warmup", start)
	var reviewPasses atomic.Int64
	observed := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
		if strings.Contains(query, "name: RecordPendingChangeReviewSummary") {
			reviewPasses.Add(1)
		}
	})
	ctx, cancel := context.WithCancel(observed)
	waitForTick := func(t *testing.T, tick func() error) {
		t.Helper()
		testenv.WaitUntil(t, ctx, tick)
		previous := reviewPasses.Load()
		testenv.WaitUntil(t, ctx, func() error {
			if reviewPasses.Load() <= previous {
				return fmt.Errorf("maintenance has not begun its following review pass")
			}
			return nil
		})
	}
	s.workerDone = make(chan struct{})
	s.workerCancel = cancel
	t.Cleanup(func() {
		cancel()
		<-s.workerDone
		s.requestMetrics = nil
	})
	go s.runPostgresMaintenance(ctx)
	waitForTick(t, warmup)
	for _, test := range []struct {
		name     string
		after    time.Duration
		commands int
	}{
		{"first minute", time.Minute, 202},
		{"second minute", 2 * time.Minute, 203},
		{"backfill finishes", 3 * time.Minute, 204},
		{"backfill stays complete", 4 * time.Minute, 205},
		{"before cleanup boundary", time.Hour - time.Nanosecond, 206},
		{"cleanup boundary", time.Hour, 0},
		{"minute after cleanup", time.Hour + time.Minute, 1},
		{"delayed tick", 3 * time.Hour, 0},
		{"minute after delayed tick", 3*time.Hour + time.Minute, 1},
		{"next cleanup", 4 * time.Hour, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := start.Add(test.after)
			insertCommand(t, f.project.OrganizationID, test.name, at)
			var deliveries []string
			for _, attempts := range []int64{0, 1} {
				payload, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID}, []byte(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				row := projectstore.GitHubWebhook{DeliveryID: fmt.Sprintf("%s-%d", test.name, attempts), Repository: "owner/repository", EventType: "ping", Digest: artifact.Digest([]byte(`{}`)), Payload: payload, ReceivedAt: at, ExpiresAt: at.Add(time.Hour), NextAttemptAt: at, Status: "pending", Attempts: attempts}
				if err := s.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					owner := w.(projectstore.GitHubIntakeWriter)
					created, err := owner.CreateGitHubWebhook(t.Context(), row)
					if err != nil || !created {
						return fmt.Errorf("create unique cadence delivery: created=%t: %v", created, err)
					}
					if attempts != 0 {
						if err := owner.UpdateGitHubWebhook(t.Context(), row, "", 0); err != nil {
							return err
						}
					}
					stored, err := owner.GitHubWebhook(t.Context(), row.DeliveryID)
					if err != nil || stored.Attempts != attempts || stored.Status != "pending" {
						return fmt.Errorf("cadence delivery lost initial retry state: attempts=%d status=%s: %v", stored.Attempts, stored.Status, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				deliveries = append(deliveries, row.DeliveryID)
			}
			tick := metricTick(t, test.name, at)
			clock.Store(at.UnixNano())
			for range 2 {
				if err := s.maintainGitHubWebhookRecords(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(projectstore.GitHubIntakeReader)
				for _, id := range deliveries {
					row, err := owner.GitHubWebhook(t.Context(), id)
					if err != nil || row.Status != "completed" || row.Attempts != 0 {
						return fmt.Errorf("unprocessed webhook delivery or retry %s: status=%s attempts=%d: %v", id, row.Status, row.Attempts, err)
					}
				}
				_, err := owner.NextGitHubWebhook(t.Context(), at)
				if !errors.Is(err, corestore.ErrNotFound) {
					return fmt.Errorf("webhook work remains after bounded steps: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			waitForTick(t, func() error {
				if err := tick(); err != nil {
					return err
				}
				commands, err := readCommands(f.project.OrganizationID)
				if err != nil || commands != test.commands {
					return fmt.Errorf("commands=%d, want %d: %v", commands, test.commands, err)
				}
				return nil
			})
			if remaining, err := readCommands(foreign); err != nil || remaining != 1 {
				t.Fatalf("current retention changed foreign receipt: remaining=%d: %v", remaining, err)
			}
			archived := readWorkItem(t, f, issue.WorkItemID, "").Archived
			if want := test.after >= time.Hour; archived != want {
				t.Errorf("archived=%t, want %t", archived, want)
			}
		})
	}
}

func TestNativeRetentionCompletedBackfillsAvoidWriter(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	s := f.service
	item := f.create(t, "historical checkpoint projection")
	content := eventstore.NewContent(s.runDetailStore())
	value := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: item.WorkItemID, Type: "run.checkpointed", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-principal"}, RecordedAt: s.config.now()}
	row, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	row.PushKnown = false
	if err := s.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), row); return err }); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"claim facts", s.populateClaimRecoveryProjections},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if test.name == "claim facts" {
				var projected bool
				if err := usagePostgresAdmin(t, s).QueryRowContext(t.Context(), "SELECT push_known FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, row.ID).Scan(&projected); err != nil || !projected {
					t.Fatalf("historical checkpoint projection = %t: %v", projected, err)
				}
			}
			var mu sync.Mutex
			statements, writes := 0, 0
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
				mu.Lock()
				defer mu.Unlock()
				statements++
				query = strings.ToUpper(strings.TrimSpace(query))
				for strings.HasPrefix(query, "--") {
					_, query, _ = strings.Cut(query, "\n")
					query = strings.TrimSpace(query)
				}
				for _, prefix := range []string{"INSERT ", "UPDATE ", "DELETE ", "CREATE ", "ALTER ", "DROP ", "TRUNCATE "} {
					if strings.HasPrefix(query, prefix) {
						writes++
					}
				}
			})
			if err := test.run(ctx); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			observedStatements, observedWrites := statements, writes
			mu.Unlock()
			if observedStatements == 0 || observedWrites != 0 {
				t.Fatalf("completed facts SQL: statements=%d writes=%d, want observed statements and no writes", observedStatements, observedWrites)
			}
		})
	}
}

func frozenRetentionTimeout(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

func TestNativeRetentionStageBudgets(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		expire   bool
		injected bool
	}{
		{name: "stage completes before deadline"},
		{name: "stage deadline expires", expire: true},
		{name: "injected timer controls deadline", injected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Service{}
				start := time.Now()
				budget := 2 * time.Second
				if test.injected {
					budget = -time.Second
					s.config.retentionTimeout = func(ctx context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
						if duration != 2*time.Second {
							t.Fatalf("stage budget=%v", duration)
						}
						return context.WithDeadline(ctx, start.Add(budget))
					}
				}
				var firstErr func() error
				nextRan := false
				err := s.runNativeRetentionStages(t.Context(), []nativeRetentionStage{
					{"first", func(ctx context.Context) error {
						firstErr = ctx.Err
						deadline, ok := ctx.Deadline()
						if !ok || deadline.Sub(start) != budget {
							t.Fatalf("stage deadline=%v present=%t", deadline, ok)
						}
						if test.expire {
							<-ctx.Done()
						}
						return ctx.Err()
					}},
					{"next", func(ctx context.Context) error {
						nextRan = true
						if firstErr() == nil {
							t.Error("completed stage context remains active")
						}
						return ctx.Err()
					}},
				})
				if test.expire || test.injected {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("expired stage error=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if !nextRan {
					t.Fatal("one stage prevented the next stage")
				}
			})
		})
	}
}
