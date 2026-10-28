package runtimeoutput

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		limit         int
		want          string
		wantTruncate  bool
		wantStrategy  string
		wantOriginal  int
		wantStoredMax int
	}{
		{
			name:  "disabled preserves output",
			input: "hello world",
			limit: 0,
			want:  "hello world",
		},
		{
			name:         "normal output under limit",
			input:        "hello world",
			limit:        64,
			want:         "hello world",
			wantOriginal: len("hello world"),
		},
		{
			name:         "exact boundary output",
			input:        "1234567890",
			limit:        10,
			want:         "1234567890",
			wantOriginal: len("1234567890"),
		},
		{
			name:          "oversized output preserves head and tail",
			input:         "0123456789abcdefghijklmnopqrstuvwxyz",
			limit:         len(Marker) + 10,
			want:          "01234" + Marker + "vwxyz",
			wantTruncate:  true,
			wantStrategy:  StrategyHeadTail,
			wantOriginal:  len("0123456789abcdefghijklmnopqrstuvwxyz"),
			wantStoredMax: len(Marker) + 10,
		},
		{
			name:          "multi-byte output remains valid utf8",
			input:         strings.Repeat("界", 10) + "abcdef" + strings.Repeat("尾", 10),
			limit:         len(Marker) + len("界界") + len("尾尾"),
			want:          "界界" + Marker + "尾尾",
			wantTruncate:  true,
			wantStrategy:  StrategyHeadTail,
			wantOriginal:  len(strings.Repeat("界", 10) + "abcdef" + strings.Repeat("尾", 10)),
			wantStoredMax: len(Marker) + len("界界") + len("尾尾"),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Truncate(tt.input, tt.limit)
			if got.Value != tt.want {
				t.Fatalf("Value = %q, want %q", got.Value, tt.want)
			}
			if !utf8.ValidString(got.Value) {
				t.Fatalf("Value is not valid UTF-8: %q", got.Value)
			}
			if (got.Truncation != nil) != tt.wantTruncate {
				t.Fatalf("Truncation present = %v, want %v", got.Truncation != nil, tt.wantTruncate)
			}
			if got.Truncation == nil {
				return
			}
			if !got.Truncation.Truncated {
				t.Fatal("Truncation.Truncated = false, want true")
			}
			if got.Truncation.OriginalBytes != tt.wantOriginal {
				t.Fatalf("OriginalBytes = %d, want %d", got.Truncation.OriginalBytes, tt.wantOriginal)
			}
			if got.Truncation.StoredBytes != len(got.Value) {
				t.Fatalf("StoredBytes = %d, want %d", got.Truncation.StoredBytes, len(got.Value))
			}
			if got.Truncation.StoredBytes > tt.wantStoredMax {
				t.Fatalf("StoredBytes = %d, want at most %d", got.Truncation.StoredBytes, tt.wantStoredMax)
			}
			if got.Truncation.LimitBytes != tt.limit {
				t.Fatalf("LimitBytes = %d, want %d", got.Truncation.LimitBytes, tt.limit)
			}
			if got.Truncation.Strategy != tt.wantStrategy {
				t.Fatalf("Strategy = %q, want %q", got.Truncation.Strategy, tt.wantStrategy)
			}
		})
	}
}

func TestBufferTruncatesIncrementalOutput(t *testing.T) {
	t.Parallel()

	buffer := NewBuffer(Policy{MaxBytes: len(Marker) + 10})
	buffer.Append("0123")
	buffer.Append("456789abcdefghijklmnopqrstuvwxyz")

	got := buffer.Text()
	want := "01234" + Marker + "vwxyz"
	if got.Value != want {
		t.Fatalf("Value = %q, want %q", got.Value, want)
	}
	if got.Truncation == nil {
		t.Fatal("Truncation = nil, want metadata")
	}
	if got.Truncation.OriginalBytes != len("0123456789abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("OriginalBytes = %d, want full input length", got.Truncation.OriginalBytes)
	}
}

func TestBufferDoesNotBackfillHeadAfterMultibyteBoundary(t *testing.T) {
	t.Parallel()

	buffer := NewBuffer(Policy{MaxBytes: len(Marker) + 10})
	buffer.Append("abcd")
	buffer.Append("界" + strings.Repeat("x", 30))
	buffer.Append("tail!")

	got := buffer.Text()
	want := "abcd" + Marker + "tail!"
	if got.Value != want {
		t.Fatalf("Value = %q, want %q", got.Value, want)
	}
	if !utf8.ValidString(got.Value) {
		t.Fatalf("Value is not valid UTF-8: %q", got.Value)
	}
	if got.Truncation == nil {
		t.Fatal("Truncation = nil, want metadata")
	}
	if got.Truncation.StoredBytes > got.Truncation.LimitBytes {
		t.Fatalf("StoredBytes = %d, want at most %d", got.Truncation.StoredBytes, got.Truncation.LimitBytes)
	}
}

func TestDiagnostic(t *testing.T) {
	for _, limit := range []int{4096, 64 * 1024} {
		for _, test := range []struct {
			name  string
			input string
			want  []string
		}{
			{name: "mixed diagnostics", input: "assertion setup\nAssertionError: expected range\nexpected selection\nactual null\n" + strings.Repeat("assertion detail\n", 12) + "diagnostic region end\n" + strings.Repeat("ok example.org/passing\n", 8000) + "--- FAIL: TestLater (0.01s)\n", want: []string{"assertion setup\n", "AssertionError: expected range\n", "expected selection\n", "actual null\n", "diagnostic region end\n", "--- FAIL: TestLater (0.01s)\n"}},
			{name: "separated failures", input: "--- FAIL: TestFirst (0.01s)\n" + strings.Repeat("ok example.org/passing\n", 4000) + "--- FAIL: TestSecond (0.02s)\n" + strings.Repeat("ok example.org/passing\n", 4000) + "FAIL\texample.org/second\t1s\n", want: []string{"--- FAIL: TestFirst (0.01s)\n", "--- FAIL: TestSecond (0.02s)\n", "FAIL\texample.org/second\t1s\n"}},
			{name: "failure at EOF stays a complete line", input: strings.Repeat("ok example.org/passing\n", 4000) + "test timed out after 1m0s", want: []string{"test timed out after 1m0s\n"}},
			{name: "failure overflow", input: strings.Repeat("--- FAIL: TestExample (0.01s)\n", limit), want: []string{"failure lines dropped]\n"}},
			{name: "oversized failure before short failure", input: "panic: " + strings.Repeat("界", limit) + "\n--- FAIL: TestShort (0.01s)\n", want: []string{"--- FAIL: TestShort (0.01s)\n", "[1 failure lines dropped]\n"}},
		} {
			t.Run(fmt.Sprintf("%s/%d", test.name, limit), func(t *testing.T) {
				got, truncated := Diagnostic(test.input, limit)
				if !truncated || len(got) > limit || !utf8.ValidString(got) {
					t.Fatalf("invalid diagnostic bounds: bytes=%d truncated=%v", len(got), truncated)
				}
				for _, want := range test.want {
					if !strings.Contains(got, want) {
						t.Errorf("missing %q", want)
					}
				}
				if test.name == "failure overflow" {
					kept := strings.Count(got, "--- FAIL: TestExample (0.01s)\n")
					want := fmt.Sprintf("[%d failure lines dropped]\n", limit-kept)
					if !strings.HasSuffix(got, want) || kept == 0 || strings.Count(got, "--- FAIL") != kept || limit-len(got) >= len("--- FAIL: TestExample (0.01s)\n") {
						t.Fatalf("incomplete failure lines or incorrect dropped count: kept=%d", kept)
					}
					if limit == 64*1024 {
						public, _ := Diagnostic(got, 4096)
						publicKept := strings.Count(public, "--- FAIL: TestExample (0.01s)\n")
						if !strings.HasSuffix(public, fmt.Sprintf("[%d failure lines dropped]\n", limit-publicKept)) {
							t.Fatal("second truncation lost cumulative dropped count")
						}
					}
				}
			})
		}
	}
}
