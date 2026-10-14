package runnerauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRunnerDiagnosticPages(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 20, 0, 0, time.UTC)
	d := &ProjectDiagnostics{Source: "runner_runtime_and_durable_attempt_owners", ObservedAt: now, Counts: map[string]int{}, Unavailable: map[string]string{}}
	for i := range DiagnosticRecordLimit + 1 {
		d.Records = append(d.Records, DiagnosticAttempt{Key: fmt.Sprintf("local:%04d", i), IssueID: "wi_test", LocalAttemptID: int64(i + 1), Membership: []string{"durable_active"}, LatestError: "token=secret https://user:key@private/ prompt: customer text [private error text omitted]", Unavailable: map[string]string{}})
	}
	d.Bound()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		source *ProjectDiagnostics
		at     time.Time
		status string
	}{{"current", d, now, "available"}, {"stale", d, now.Add(HeartbeatTimeout + time.Second), "stale"}, {"older", nil, now, "unavailable"}} {
		t.Run(test.name, func(t *testing.T) {
			p, err := test.source.Page("p", "r", "", "", "", 3, test.at)
			if err != nil || p.Status != test.status {
				t.Fatalf("page=%+v err=%v", p, err)
			}
		})
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		p, err := d.Page("p", "r", "", "", cursor, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Truncated {
			t.Fatal("lost source truncation")
		}
		for _, r := range p.Records {
			if seen[r.Key] {
				t.Fatal("duplicate across pages")
			}
			seen[r.Key] = true
		}
		cursor = p.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != DiagnosticRecordLimit {
		t.Fatalf("records=%d", len(seen))
	}
	p, err := d.Page("p", "r", "wi_unrecorded", "", "", 5, now)
	if err != nil || p.Unavailable["admission_decision"] == "" {
		t.Fatalf("missing unavailable decision: %+v %v", p, err)
	}
	if _, err := d.Page("p", "r", "", "", "missing", 5, now); err == nil {
		t.Fatal("accepted unknown cursor")
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"secret", "customer", "https://", "user:key"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("disclosed %s", private)
		}
	}
}
