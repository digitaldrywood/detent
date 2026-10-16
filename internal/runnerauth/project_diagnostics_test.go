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
	for _, test := range []struct{ name, value, want string }{
		{"known", "publication rejected: category=runtime_validation field=data.runtime.activity [private error text omitted]", "publication rejected: category=runtime_validation field=data.runtime.activity [private error text omitted]"},
		{"unknown category", "publication rejected: category=sentinel-secret field=data.runtime.activity [private error text omitted]", "[private error text omitted]"},
		{"forged field", "publication rejected: category=runtime_validation field=sentinel-secret [private error text omitted]", "[private error text omitted]"},
		{"prefix", "sentinel-secret publication rejected: category=runtime_validation field=data.runtime.activity [private error text omitted]", "[private error text omitted]"},
		{"suffix", "publication rejected: category=runtime_validation field=data.runtime.activity [private error text omitted] sentinel-secret", "[private error text omitted]"},
		{"unclassified", "sentinel-secret", "[private error text omitted]"},
		{"unavailable publication", "sentinel-secret", "[private error text omitted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := &ProjectDiagnostics{Records: []DiagnosticAttempt{{LatestErrorCode: "invalid_request", LatestError: test.value}}}
			if test.name == "unavailable publication" {
				snapshot.Records[0].LatestErrorCode = "unavailable"
				snapshot.Records[0].OperationPending = true
				snapshot.Records[0].HostOperation = "native.publishVersion"
			}
			snapshot.Redact()
			snapshot.Redact()
			r := snapshot.Records[0]
			if r.LatestError != test.want {
				t.Fatalf("error = %q", r.LatestError)
			}
			if test.name != "known" && r.Unavailable["publication_rejection"] != "unavailable" {
				t.Fatal("unknown rejection not marked unavailable")
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
