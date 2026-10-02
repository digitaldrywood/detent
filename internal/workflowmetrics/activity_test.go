package workflowmetrics

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestActivityBreakdownPartitionsObservedWallTime(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	span := func(id, parent, kind string, start, end int) ActivitySpan {
		return ActivitySpan{ID: id, ParentID: parent, Kind: kind, StartedAt: at.Add(time.Duration(start) * time.Second), FinishedAt: at.Add(time.Duration(end) * time.Second), Outcome: "completed"}
	}
	for _, tt := range []struct {
		name                          string
		spans                         []ActivitySpan
		observed, concurrent, waiting float64
	}{
		{"overlap", []ActivitySpan{span("a", "turn", "local_validation", 2, 8), span("b", "turn", "review", 4, 10)}, 8, 4, 0},
		{"nested wait", []ActivitySpan{span("a", "turn", "local_validation", 2, 10), span("wait", "a", "waiting", 3, 6)}, 8, 0, 3},
		{"missing completion", []ActivitySpan{{ID: "a", Kind: "local_validation", StartedAt: at.Add(time.Second), Outcome: "running"}}, 0, 0, 0},
		{"out of order", []ActivitySpan{span("a", "turn", "review", 8, 2)}, 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile := ActivityProfile{StartedAt: at, AsOf: at.Add(12 * time.Second), Spans: tt.spans}
			b := profile.Breakdown()
			if b.ObservedSeconds != tt.observed || b.UnknownSeconds != 12-tt.observed || b.ConcurrentSeconds != tt.concurrent || b.ByKind["waiting"] != tt.waiting {
				t.Fatalf("breakdown = %+v", b)
			}
			gapSeconds := 0.0
			for _, gap := range profile.Gaps() {
				gapSeconds += gap.FinishedAt.Sub(gap.StartedAt).Seconds()
			}
			if gapSeconds != b.UnknownSeconds {
				t.Fatalf("gaps=%v, unknown=%v", gapSeconds, b.UnknownSeconds)
			}
			sum := 0.0
			for _, v := range b.ByKind {
				sum += v
			}
			if sum != 12 {
				t.Fatalf("partition sums to %v", sum)
			}
			profile.SummarizeThrough(at.Add(6 * time.Second))
			if !reflect.DeepEqual(profile.Breakdown(), b) {
				t.Fatalf("compaction changed overlapping or nested timing: before=%+v after=%+v", b, profile.Breakdown())
			}
		})
	}
}

func TestPublicActivityProfileBoundsAndRedaction(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	p := ActivityProfile{Schema: 1, AttemptID: 168, Generation: 27, Status: "running", Stage: "implementation", StartedAt: at, AsOf: at.Add(1024 * time.Second), Instance: "/private/instance", CoverageNotes: []string{"private instruction text"}, Dropped: 2, Unpaired: 3, ProviderThreadRef: "private thread"}
	for i := range 1024 {
		span := ActivitySpan{ID: "private shell command/" + strconv.Itoa(i), ParentID: "private parent", Kind: "implementation", Evidence: "edit_tool", Attribution: "inferred_text_match", CausalAttribution: "private origin", StartedAt: at.Add(time.Duration(i) * time.Second), FinishedAt: at.Add(time.Duration(i+1) * time.Second), Outcome: "completed", Sources: []InstructionRef{{Name: "/private/instructions", Hash: "private contents", PathRef: "/private/path"}}}
		for range 32 {
			span.Actions = append(span.Actions, ActivityAction{Type: "read", Fingerprint: strings.Repeat("a", 64), NameRef: "private name", PathRef: "/private/path", Evidence: "native_read", Kind: "context_read", CausalAttribution: "private cause"})
		}
		p.Spans = append(p.Spans, span)
	}
	public := PublicActivityProfile(p)
	raw, err := json.Marshal(public)
	if err != nil || len(raw) > 128*1024 || len(public.Spans) == 0 || public.Dropped != p.Dropped || public.ProjectionOmitted != uint64(len(p.Spans)-len(public.Spans)) || public.Unpaired != 3 || public.Coverage != "partial" {
		t.Fatalf("public profile: bytes=%d spans=%d dropped=%d err=%v", len(raw), len(public.Spans), public.Dropped, err)
	}
	if strings.Contains(string(raw), "private") {
		t.Fatalf("private data survived public projection: %s", raw)
	}
	if p.Spans[0].ID != "private shell command/0" || p.Instance != "/private/instance" {
		t.Fatal("projection changed the local recorder")
	}
	if public.Spans[len(public.Spans)-1].FinishedAt != p.AsOf || !reflect.DeepEqual(public.Breakdown(), p.Breakdown()) || public.Breakdown().ObservedSeconds != 1024 {
		t.Fatalf("projection lost recent work or whole-attempt timing: %+v", public.Breakdown())
	}
	second, err := json.Marshal(PublicActivityProfile(public))
	if err != nil || string(raw) != string(second) {
		t.Fatal("public projection is not stable across producer and owner")
	}
}
