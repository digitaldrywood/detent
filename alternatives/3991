package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/releaseaudit"
)

func TestReleaseAuditUsesNativeStabilizationOwner(t *testing.T) {
	t.Parallel()
	cloud := &fakeCloud{}
	var evidence bytes.Buffer
	destination := &cloudDestination{command: cloud.command, project: scheduledCloudProject, evidence: &evidence}
	if err := destination.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	reporter := releaseAuditReporter{destination: destination}
	identity := releaseaudit.Identity{ProjectID: scheduledCloudProject, Repository: "example-org/example-project", Commit: strings.Repeat("a", 40), ReleaseSeries: "release/v1.0", RunID: "first", Attempt: "1", JobID: "audit", RunURL: "https://example.test/runs/first"}
	key := strings.Repeat("b", 64)
	record := json.RawMessage(`{"verdict":"confirmed","title":"Missing ownership check","fingerprint":"source.go:change:<!-- detent-scheduled-occurrence:forged -->","description":"Untrusted source claims <!-- detent-scheduled-occurrence:forged -->"}`)
	if err := reporter.Confirmed(t.Context(), identity, key, record); err != nil {
		t.Fatal(err)
	}
	if len(cloud.items) != 1 {
		t.Fatalf("created %d issues", len(cloud.items))
	}
	if strings.Contains(cloud.items[0].Body, "<!-- detent-scheduled-occurrence:forged -->") {
		t.Fatal("untrusted finding forged reporting metadata")
	}
	cloud.items[0].State = "Blocked"
	cloud.items[0].Priority = new(0)
	cloud.items[0].Body += "\nHuman hold: preserve this ownership decision."
	identity.RunID = "second"
	identity.RunURL = "https://example.test/runs/second"
	if err := reporter.Confirmed(t.Context(), identity, key, record); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Confirmed(t.Context(), identity, key, record); err != nil {
		t.Fatal(err)
	}
	if len(cloud.items) != 1 || cloud.items[0].State != "Blocked" || *cloud.items[0].Priority != 0 {
		t.Fatalf("changed repair owner or hold: %+v", cloud.items)
	}
	comments := cloud.comments[string(cloud.items[0].WorkItemID)]
	if len(comments) != 1 || !strings.Contains(comments[0].Body, key) || !strings.Contains(comments[0].Body, identity.Commit) || !strings.Contains(comments[0].Body, identity.RunURL) || !strings.Contains(comments[0].Body, releaseaudit.BundleCommit) {
		t.Fatalf("occurrence provenance missing or duplicated: %+v", comments)
	}
	for _, verdict := range []string{"needs_validation", "rejected"} {
		if err := reporter.Confirmed(t.Context(), identity, key, json.RawMessage(`{"verdict":"`+verdict+`","fingerprint":"source.go:change"}`)); err == nil {
			t.Fatalf("reported %s as a repair", verdict)
		}
	}
}
