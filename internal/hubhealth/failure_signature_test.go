package hubhealth

import (
	"strings"
	"testing"
	"unicode/utf8"
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
