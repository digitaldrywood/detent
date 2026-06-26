package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSSEStreamSkipsUnchangedFragments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	stream := newTestSSEStream(&now)
	var out bytes.Buffer

	sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "<section>same</section>",
		payloadBytes: len("<section>same</section>"),
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("sendRendered() error = %v", err)
	}
	if !sent {
		t.Fatal("first sendRendered() sent = false, want true")
	}

	now = now.Add(time.Second)
	sent, err = stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "<section>same</section>",
		payloadBytes: len("<section>same</section>"),
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("sendRendered() unchanged error = %v", err)
	}
	if sent {
		t.Fatal("unchanged sendRendered() sent = true, want false")
	}
	if got := strings.Count(out.String(), "event: snapshot"); got != 1 {
		t.Fatalf("snapshot event count = %d, want 1; output:\n%s", got, out.String())
	}
	metrics := stream.metricsFor(sseEventSnapshot)
	if metrics.skippedUnchanged != 1 {
		t.Fatalf("skippedUnchanged = %d, want 1", metrics.skippedUnchanged)
	}
}

func TestSSEStreamCoalescesPendingFragments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	stream := newTestSSEStream(&now)
	var out bytes.Buffer

	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "initial",
		payloadBytes: len("initial"),
	}, 5*time.Second); err != nil {
		t.Fatalf("sendRendered() initial error = %v", err)
	} else if !sent {
		t.Fatal("initial sendRendered() sent = false, want true")
	}

	now = now.Add(time.Second)
	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "stale",
		payloadBytes: len("stale"),
	}, 5*time.Second); err != nil {
		t.Fatalf("sendRendered() stale error = %v", err)
	} else if sent {
		t.Fatal("stale sendRendered() sent = true, want coalesced")
	}

	now = now.Add(time.Second)
	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "latest",
		payloadBytes: len("latest"),
	}, 5*time.Second); err != nil {
		t.Fatalf("sendRendered() latest error = %v", err)
	} else if sent {
		t.Fatal("latest sendRendered() sent = true, want coalesced")
	}

	now = now.Add(3 * time.Second)
	if sent, err := stream.flushPending(context.Background(), &out); err != nil {
		t.Fatalf("flushPending() error = %v", err)
	} else if !sent {
		t.Fatal("flushPending() sent = false, want true")
	}

	output := out.String()
	if !strings.Contains(output, "data: latest") {
		t.Fatalf("coalesced output missing latest body:\n%s", output)
	}
	if strings.Contains(output, "data: stale") {
		t.Fatalf("coalesced output sent stale body:\n%s", output)
	}
	if got := strings.Count(output, "event: snapshot"); got != 2 {
		t.Fatalf("snapshot event count = %d, want 2; output:\n%s", got, output)
	}
	metrics := stream.metricsFor(sseEventSnapshot)
	if metrics.coalesced != 2 {
		t.Fatalf("coalesced = %d, want 2", metrics.coalesced)
	}
	if metrics.sent != 2 {
		t.Fatalf("sent = %d, want 2", metrics.sent)
	}
}

func TestSSEStreamLogsMetricsByEvent(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	stream := newSSEStream(logger, time.Second)
	stream.startedAt = now
	stream.nextMetricsAt = now.Add(time.Second)
	stream.now = func() time.Time { return now }
	var out bytes.Buffer

	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "payload",
		payloadBytes: len("payload"),
	}, 0); err != nil {
		t.Fatalf("sendRendered() error = %v", err)
	} else if !sent {
		t.Fatal("sendRendered() sent = false, want true")
	}

	now = now.Add(time.Second)
	stream.logMetricsIfDue(now)

	for _, want := range []string{
		"dashboard sse stream metrics",
		"event=snapshot",
		"sent_per_second",
		"sent_payload_bytes",
		"rendered_payload_bytes",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("metrics log missing %q:\n%s", want, logs.String())
		}
	}
}

func newTestSSEStream(now *time.Time) *sseStream {
	stream := newSSEStream(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	stream.startedAt = now.UTC()
	stream.nextMetricsAt = now.Add(time.Hour)
	stream.now = func() time.Time { return now.UTC() }
	return stream
}
