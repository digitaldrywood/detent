package hubserver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRetentionWebhookMaintenanceCadence(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	now := start
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }, retentionTimeout: frozenRetentionTimeout})
	s := f.service
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := s.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	issue := f.create(t, "closed issue")
	tx, err := s.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issue.State = "Done"
	if _, err := persistNativeIssue(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,?,'slots','example',3,'{}')`, f.project.OrganizationID, f.project.ID, now.Add(-31*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	insertCommand := func(key string, pending any) {
		t.Helper()
		_, err := s.database.db.ExecContext(t.Context(), `INSERT INTO native_commands
(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending)
VALUES(?,?,'POST /example',?,'hash','{}',?,?)`, f.project.OrganizationID, defaultHubOperatorID, key, formatHubTime(now.Add(-25*time.Hour)), pending)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range 201 {
		insertCommand(fmt.Sprintf("backfill-%d", i), nil)
	}
	for _, test := range []struct {
		name       string
		after      time.Duration
		commands   int
		unfinished int
	}{
		{"first minute", time.Minute, 202, 101},
		{"second minute", 2 * time.Minute, 203, 1},
		{"backfill finishes", 3 * time.Minute, 204, 0},
		{"backfill stays complete", 4 * time.Minute, 205, 0},
		{"before cleanup boundary", time.Hour - time.Nanosecond, 206, 0},
		{"cleanup boundary", time.Hour, 0, 0},
		{"minute after cleanup", time.Hour + time.Minute, 1, 0},
		{"delayed tick", 3 * time.Hour, 0, 0},
		{"minute after delayed tick", 3*time.Hour + time.Minute, 1, 0},
		{"next cleanup", 4 * time.Hour, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			now = start.Add(test.after)
			insertCommand(test.name, false)
			for _, status := range []string{"pending", "failed"} {
				receipt, err := s.database.recordWebhook(t.Context(), webhookReceipt{
					DeliveryID: test.name + "-" + status, EventType: "ping", HeadersJSON: "{}",
					Payload: []byte("{}"), PayloadSHA256: "digest", ReceivedAt: now, PayloadExpiresAt: now.Add(time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.database.db.ExecContext(t.Context(), "UPDATE github_webhook_inbox SET status=?,next_retry_at=? WHERE id=?", status, formatWebhookTime(now), receipt.InboxID); err != nil {
					t.Fatal(err)
				}
			}
			s.maintainGitHubWebhooks(t.Context())
			var commands, unfinished, pending int
			if err := s.database.reader.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER (WHERE response_pending IS NULL) FROM native_commands WHERE operation='POST /example'`).Scan(&commands, &unfinished); err != nil {
				t.Fatal(err)
			}
			if commands != test.commands || unfinished != test.unfinished {
				t.Errorf("commands=%d unfinished=%d, want %d and %d", commands, unfinished, test.commands, test.unfinished)
			}
			if err := s.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM github_webhook_inbox WHERE status<>'ignored'").Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != 0 {
				t.Errorf("unprocessed webhook deliveries and retries=%d", pending)
			}
			archived := readWorkItem(t, f, issue.WorkItemID, "").Archived
			if want := test.after >= time.Hour; archived != want {
				t.Errorf("archived=%t, want %t", archived, want)
			}
			var samples, summaries int
			if err := s.database.reader.QueryRowContext(t.Context(), `SELECT
(SELECT count(*) FROM analytics_hourly_rollups WHERE project_id=? AND metric='slots'),
(SELECT count(*) FROM analytics_sample_hours WHERE project_id=? AND metric='slots')`, f.project.ID, f.project.ID).Scan(&samples, &summaries); err != nil {
				t.Fatal(err)
			}
			wantSamples, wantSummaries := 1, 0
			if test.after >= time.Hour {
				wantSamples, wantSummaries = 0, 1
			}
			if samples != wantSamples || summaries != wantSummaries {
				t.Errorf("samples=%d summaries=%d, want %d and %d", samples, summaries, wantSamples, wantSummaries)
			}
		})
	}
}

func TestNativeRetentionCompletedBackfillsAvoidWriter(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	s := f.service
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := s.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"conversation metadata", s.convertConversationQueryMetadata},
		{"claim facts", s.database.populateClaimFacts},
		{"runner facts", s.database.populateRunnerFacts},
		{"archive facts", s.database.populateArchiveFacts},
		{"hosted import fields", s.database.populateHostedImportFields},
		{"time indexes", s.database.populateTimeIndexes},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			ctx, timing := withWriterTiming(t.Context())
			if err := test.run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := timing.txs.Load(); got != 0 {
				t.Fatalf("completed backfill used the writer %d times", got)
			}
		})
	}
}

func frozenRetentionTimeout(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

func TestNativeRetentionStageBudgets(t *testing.T) {
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
