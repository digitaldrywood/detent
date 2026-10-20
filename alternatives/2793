package runtimeoutput

import (
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
