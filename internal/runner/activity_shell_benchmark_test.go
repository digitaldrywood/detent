package runner

import (
	"strings"
	"testing"
)

// Pure parsing/classification only: no shell execution, files, provider calls,
// recorder goroutines or persistence. Compare the existing classifier alone
// with the decoder followed by that same classifier.
func BenchmarkActivityShellClassification(b *testing.B) {
	for _, input := range []struct{ name, command string }{
		{"plain", "go test ./internal/foo"},
		{"native_read", `/bin/zsh -lc 'cat "AGENTS.md"'`},
		{"native_validation", `/bin/zsh -lc 'go test ./internal/foo'`},
		{"expanding", `/bin/zsh -lc "go test $PACKAGE"`},
		{"bounded", "/bin/zsh -lc 'go test " + strings.Repeat("x", 8192-len("/bin/zsh -lc 'go test '")) + "'"},
	} {
		b.Run(input.name, func(b *testing.B) {
			b.Run("classifier", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					classifyActivity("commandExecution", input.command)
				}
			})
			b.Run("decoded", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					classifyActivity("commandExecution", activityShellCommand(input.command))
				}
			})
		})
	}
}
