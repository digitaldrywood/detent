package workspacegit

import (
	"strings"
	"testing"
)

func TestHeadBufferKeepsTheFirstBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		limit         int
		writes        []string
		want          string
		wantTruncated bool
	}{
		{name: "under the limit", limit: 8, writes: []string{"abc", "de"}, want: "abcde"},
		{name: "exactly the limit", limit: 5, writes: []string{"abc", "de"}, want: "abcde"},
		{name: "one write past the limit", limit: 4, writes: []string{"abcdef"}, want: "abcd", wantTruncated: true},
		{name: "later writes past the limit", limit: 4, writes: []string{"abc", "def", "ghi"}, want: "abcd", wantTruncated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			buffer := &headBuffer{limit: test.limit}
			for _, write := range test.writes {
				if n, err := buffer.Write([]byte(write)); n != len(write) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want a full write", write, n, err)
				}
			}
			if got := buffer.buffer.String(); got != test.want || buffer.truncated != test.wantTruncated {
				t.Fatalf("kept %q truncated %v, want %q truncated %v", got, buffer.truncated, test.want, test.wantTruncated)
			}
		})
	}
}

func TestTailBufferKeepsTheLastBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{name: "under the limit", limit: 16, writes: []string{"fatal: ", "nope\n"}, want: "fatal: nope"},
		{name: "past the limit", limit: 4, writes: []string{"progress ", "verdict\n"}, want: "[earlier output omitted]\nict"},
		{name: "one large write", limit: 3, writes: []string{strings.Repeat("x", 100) + "end"}, want: "[earlier output omitted]\nend"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			buffer := &tailBuffer{limit: test.limit}
			for _, write := range test.writes {
				if n, err := buffer.Write([]byte(write)); n != len(write) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want a full write", write, n, err)
				}
			}
			if got := buffer.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
			if len(buffer.data) > test.limit {
				t.Fatalf("kept %d bytes, limit %d", len(buffer.data), test.limit)
			}
		})
	}
}
