package hubserver

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func TestSummaryAttentionKeepsHumanActionsAndWatchLimitsIndependent(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, state  string
		age          time.Duration
		needs, watch int
	}{{"young work", "In Progress", time.Hour, 1, 1}, {"old work", "In Progress", 30 * time.Hour, 1, 5}, {"review work", "Human Review", 14 * time.Hour, 100, 1}} {
		t.Run(test.name, func(t *testing.T) {
			blocked, working := "Blocked", "In Progress"
			rows := []summaryAttentionRecord{{item: corestore.WorkItem{Number: 1, Title: "Answer needed", WorkflowStateName: &blocked}, at: now.Add(-time.Hour), human: true}, {item: corestore.WorkItem{Number: 2, Title: "Long running", WorkflowStateName: &working}, at: now.Add(-31 * time.Hour), attempts: 4, spent: 40}, {item: corestore.WorkItem{Number: 3, Title: "Machine failure", WorkflowStateName: &blocked}, at: now.Add(-48 * time.Hour)}}
			for i := range 100 {
				state := test.state
				rows = append(rows, summaryAttentionRecord{item: corestore.WorkItem{Number: int64(i + 10), Title: fmt.Sprintf("Filler %03d", i), WorkflowStateName: &state}, at: now.Add(-test.age - time.Duration(i)*time.Minute)})
			}
			needs, watch := summaryAttentionEntries(rows, summaryRecipient{end: now}, summaryProjectRef{name: "Example", slug: "example"}, now.Add(-24*time.Hour), "https://example.test", "org", true)
			if len(needs) != test.needs || len(watch) != test.watch || !strings.Contains(needs[0].Title, "Answer needed") || !strings.Contains(watch[0].Title, "Long running") || !strings.Contains(watch[0].Detail, "$40.00") {
				t.Fatalf("needs=%+v watch=%+v", needs, watch)
			}
			if test.needs == 100 && (!strings.Contains(needs[1].Title, "Filler 099") || !strings.Contains(needs[99].Title, "Filler 001")) {
				t.Fatalf("review ordering=%+v", needs)
			}
			_, hidden := summaryAttentionEntries(rows, summaryRecipient{end: now}, summaryProjectRef{name: "Example", slug: "example"}, now.Add(-24*time.Hour), "https://example.test", "org", false)
			if strings.Contains(hidden[0].Detail, "$") {
				t.Fatal("hidden cost disclosed")
			}
		})
	}
}
