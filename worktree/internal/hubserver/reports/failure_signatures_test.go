package reports

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestNormalizeFailureSignature(t *testing.T) {
	for _, test := range []struct {
		name, a, b, want string
	}{
		{"ids", "attempt 123 wi_abc123 lease_deadbeef", "attempt 456 wi_def456 lease_abcd1234", "attempt <id> <id> <id>"},
		{"hashes", "checkpoint deadbeef12345678 changed", "checkpoint abcdef1234567890 changed", "checkpoint <hash> changed"},
		{"numeric hashes", "checkpoint " + strings.Repeat("1", 40) + " changed", "checkpoint " + strings.Repeat("2", 64) + " changed", "checkpoint <hash> changed"},
		{"paths", "open /work/123/file42.go failed", "open /work/456/file97.go failed", "open /work/<n>/file<n>.go failed"},
		{"windows paths", `open C:\work\123\file42.go failed`, `open C:\work\456\file97.go failed`, `open C:\work\<n>\file<n>.go failed`},
		{"counts", "actual 1048576 characters exceeds limit 65536", "actual 2222222 characters exceeds limit 99999", "actual <n> characters exceeds limit <n>"},
		{"bytes", "received 1048576 bytes, needed 32 KiB", "received 2097152 bytes, needed 64 KiB", "received <bytes>, needed <bytes>"},
		{"timestamps", "failed at 2026-10-06T10:23:12.123Z", "failed at 2025-01-01T12:59:59-05:00", "failed at <timestamp>"},
		{"first line", "  local_checkpoint_changed\nprivate details", "local_checkpoint_changed\r\nother details", "local_checkpoint_changed"},
		{"bounded", strings.Repeat("界", 200), strings.Repeat("界", 200), strings.Repeat("界", 85)},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, input := range []string{test.a, test.b} {
				got := NormalizeFailureSignature(input)
				if got != test.want || len(got) > maxFailureSignatureLength || !utf8.ValidString(got) {
					t.Fatalf("normalize %q = %q, want %q", input, got, test.want)
				}
			}
		})
	}
	for _, pair := range [][2]string{{"local_checkpoint_changed", "local_checkpoint_missing"}, {"HTTP 401 unauthorized", "HTTP 403 forbidden"}, {"RPC code -32602", "RPC code -32603"}, {"open /work/123/a.go failed", "open /work/456/b.go failed"}} {
		if NormalizeFailureSignature(pair[0]) == NormalizeFailureSignature(pair[1]) {
			t.Fatalf("distinct errors collapsed: %q", pair)
		}
	}
}

func TestGroupNativeFailureSignatures(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, class, want string
		count             int
		step              time.Duration
		separateItems     bool
		olderFailure      bool
	}{
		{"sliding window", "attempt", "retry storm", 6, time.Minute, false, true},
		{"ordinary", "attempt", "attempt", 4, time.Minute, false, false},
		{"storm inclusive boundary", "attempt", "retry storm", 5, 15 * time.Minute, false, false},
		{"outside storm window", "attempt", "attempt", 5, 15*time.Minute + time.Second, false, false},
		{"different items", "attempt", "attempt", 5, time.Minute, true, false},
		{"infrastructure", "infrastructure", "infrastructure", 1, time.Minute, false, false},
		{"infrastructure storm", "infrastructure", "retry storm", 5, time.Minute, false, false},
		{"merge", "merge", "merge", 1, time.Minute, false, false},
		{"merge storm", "merge", "retry storm", 5, time.Minute, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			failures := []NativeFailure{}
			for i := range test.count {
				item := "wi_one"
				if test.separateItems {
					item = fmt.Sprintf("wi_%d", i)
				}
				at := now.Add(time.Duration(i) * test.step)
				if test.olderFailure && i == 0 {
					at = now.Add(-2 * time.Hour)
				}
				failures = append(failures, NativeFailure{WorkItem: item, Example: fmt.Sprintf("count %d failed\nsecond line", i), Class: test.class, At: at, Started: at.Add(-2 * time.Minute), Cost: 0.5, Tokens: 10})
			}
			got := GroupNativeFailures(failures)
			if len(got) != 1 {
				t.Fatalf("groups=%#v", got)
			}
			g := got[0]
			if g.Class != test.want || g.Count != test.count || g.Cost != 0.5*float64(test.count) || g.Tokens != 10*int64(test.count) || g.SlotMinutes != 2*float64(test.count) || g.FirstSeen != failures[0].At || g.LastSeen != failures[len(failures)-1].At || strings.Contains(g.Example, "\n") {
				t.Fatalf("group=%#v", g)
			}
			for i, j := 0, len(failures)-1; i < j; i, j = i+1, j-1 {
				failures[i], failures[j] = failures[j], failures[i]
			}
			if reverse := GroupNativeFailures(failures); !reflect.DeepEqual(reverse, got) {
				t.Fatalf("order-dependent groups: %#v / %#v", reverse, got)
			}
		})
	}
	t.Run("latest signature counts only matching failures", func(t *testing.T) {
		failures := []NativeFailure{
			{AttemptID: "attempt_a", Example: "count 1 failed", At: now},
			{AttemptID: "attempt_b", Example: "other error", At: now.Add(-time.Minute)},
			{AttemptID: "attempt_c", Example: "count 2 failed", At: now.Add(-time.Minute)},
		}
		got := LatestNativeFailureSignature(failures, true)
		if got == nil || got.Signature != "count <n> failed" || got.Count != 2 || !got.Partial {
			t.Fatalf("latest=%#v", got)
		}
		groups := GroupNativeFailures(failures)
		if len(groups) != 2 || groups[0].Count != 2 || groups[1].Count != 1 {
			t.Fatalf("signature grouping order=%#v", groups)
		}
		if LatestNativeFailureSignature(nil, false) != nil {
			t.Fatal("empty failures fabricated a signature")
		}
	})

	t.Run("bounded items examples and pages", func(t *testing.T) {
		failures := []NativeFailure{}
		for i := range maxFailureWorkItems + 3 {
			failures = append(failures, NativeFailure{WorkItem: fmt.Sprintf("wi_%02d", i), Example: strings.Repeat("界", 1000), Class: "attempt", At: now, Started: now.Add(time.Minute)})
		}
		got := GroupNativeFailures(failures)[0]
		if got.WorkItemsCount != len(failures) || len(got.WorkItems) != maxFailureWorkItems || !got.WorkItemsPartial || len(got.Example) > MaxFailureExampleLength || !utf8.ValidString(got.Example) || got.SlotMinutes != 0 {
			t.Fatalf("bounds=%#v", got)
		}
		groups := make([]NativeFailureSignature, 200)
		for i := range groups {
			groups[i] = got
			groups[i].WorkItems = make([]string, maxFailureWorkItems)
			for j := range groups[i].WorkItems {
				groups[i].WorkItems[j] = strings.Repeat("w", 128)
			}
		}
		page := NativeFailureSignaturePage(groups, 0, 200)
		raw, err := json.Marshal(page.Items)
		if err != nil || len(raw) > operatortool.WorkListPageBytes || page.NextOffset == nil || *page.NextOffset != len(page.Items) {
			t.Fatalf("page size=%d continuation=%v err=%v", len(raw), page.NextOffset, err)
		}
	})
}
