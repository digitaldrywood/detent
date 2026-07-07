package web

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestRunningEntriesIncludeOutputTruncationMetadata(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 7, 7, 14, 30, 0, 0, time.UTC)
	truncation := &runtimeoutput.Truncation{
		Truncated:     true,
		OriginalBytes: 36,
		StoredBytes:   29,
		LimitBytes:    29,
		OmittedBytes:  7,
		Strategy:      runtimeoutput.StrategyHeadTail,
	}

	payload := runningEntries([]telemetry.Running{{
		Issue: telemetry.Issue{
			ID:         "issue-978",
			Identifier: "digitaldrywood/detent#978",
			State:      "running",
		},
		LastMessage:           "01234" + runtimeoutput.Marker + "vwxyz",
		LastMessageTruncation: truncation,
		RecentEvents: []telemetry.ActivityEvent{{
			At:         at,
			Event:      "agent_message_delta",
			Message:    "01234" + runtimeoutput.Marker + "vwxyz",
			Truncation: truncation,
		}},
	}})

	if len(payload) != 1 {
		t.Fatalf("runningEntries() len = %d, want 1", len(payload))
	}
	if payload[0].LastMessageTruncation == nil || !payload[0].LastMessageTruncation.Truncated {
		t.Fatalf("LastMessageTruncation = %#v, want truncated metadata", payload[0].LastMessageTruncation)
	}
	if payload[0].LastMessageTruncation == truncation {
		t.Fatal("LastMessageTruncation reused source pointer")
	}
	if len(payload[0].RecentEvents) != 1 {
		t.Fatalf("RecentEvents len = %d, want 1", len(payload[0].RecentEvents))
	}
	if payload[0].RecentEvents[0].Truncation == nil || !payload[0].RecentEvents[0].Truncation.Truncated {
		t.Fatalf("RecentEvents[0].Truncation = %#v, want truncated metadata", payload[0].RecentEvents[0].Truncation)
	}
	if payload[0].RecentEvents[0].Truncation == truncation {
		t.Fatal("RecentEvents[0].Truncation reused source pointer")
	}
}
