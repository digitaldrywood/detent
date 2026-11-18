package reviewoutput

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReviewOutputRangeRetainsBoundedMemory(t *testing.T) {
	chunk := []byte(strings.Repeat("x", 32*1024))
	measurement := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			capture := Capture{Selection: Range{Offset: 32 * 1024 * 1024, Length: SourcePageBytes}}
			for range 2048 {
				if _, err := capture.Write(chunk); err != nil {
					b.Fatal(err)
				}
			}
			output, truncated := capture.Result()
			if capture.Total != 64*1024*1024 || !truncated || !strings.HasSuffix(output, strings.Repeat("x", SourcePageBytes)) {
				b.Fatal("streamed page lost source bytes or metadata")
			}
		}
	})
	if measurement.AllocedBytesPerOp() > 256*1024 {
		t.Fatalf("64 MiB stream allocated %d bytes for a 16 KiB page", measurement.AllocedBytesPerOp())
	}
}

func TestReviewOutputUTF8Pages(t *testing.T) {
	for _, fixture := range []struct {
		name, content string
		offset        int64
		length        int
	}{
		{"three byte page boundary", strings.Repeat("a", 16383) + "✓end", 0, 16384},
		{"four byte page boundary", strings.Repeat("a", 16383) + "🙂end", 0, 16384},
		{"one byte request", "✓🙂end", 0, 1},
		{"offset within rune", "✓🙂end", 1, 1},
	} {
		for _, chunk := range []int{1, 2, 32768} {
			t.Run(fmt.Sprintf("%s/chunk%d", fixture.name, chunk), func(t *testing.T) {
				var observed strings.Builder
				for offset := fixture.offset; ; {
					capture := Capture{Selection: Range{Offset: offset, Length: fixture.length}}
					for start := 0; start < len(fixture.content); start += chunk {
						if _, err := capture.Write([]byte(fixture.content[start:min(start+chunk, len(fixture.content))])); err != nil {
							t.Fatal(err)
						}
					}
					output, _ := capture.Result()
					header, content, ok := strings.Cut(output, "\n")
					var start, end, Total, next int64
					var eof bool
					if _, err := fmt.Sscanf(header, "[source bytes %d:%d of %d; next_offset=%d; eof=%t]", &start, &end, &Total, &next, &eof); err != nil {
						t.Fatal(err)
					}
					if !ok || !utf8.ValidString(content) || end-start != int64(len(content)) || Total != int64(len(fixture.content)) || next != end || end <= offset || len(content) > SourcePageBytes {
						t.Fatalf("invalid UTF-8 page: %q", output)
					}
					observed.WriteString(content)
					if eof {
						break
					}
					offset = next
				}
				if observed.String() != fixture.content {
					t.Fatalf("following next_offset lost source characters: got %q", observed.String())
				}
			})
		}
	}
}
