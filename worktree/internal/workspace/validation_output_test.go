package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
)

func TestReviewCommandOutputSurvivesJSONByteBudget(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("git and shell subprocess integration")
	}
	const limit = 64 * 1024
	for _, test := range []struct {
		name      string
		output    string
		command   string
		exit      int
		truncated bool
	}{
		{name: "short unicode", output: "verified ✓\n", command: "cat output.txt"},
		{name: "exact byte budget", output: strings.Repeat("a", limit-6) + "✓end", command: "cat output.txt"},
		{name: "three byte rune at tail boundary", output: strings.Repeat("✓", 32*1024) + "end\n", command: "cat output.txt", truncated: true},
		{name: "four byte rune at tail boundary", output: strings.Repeat("🙂", 24*1024) + "end\n", command: "cat output.txt", truncated: true},
		{name: "invalid bytes within budget", output: "before\xff\xffafter\n", command: "cat output.txt"},
		{name: "replacement crosses byte budget", output: strings.Repeat("a", limit-1) + "\xff", command: "cat output.txt", truncated: true},
		{name: "failed command preserves unicode tail", output: strings.Repeat("✓", 32*1024) + "end\n", command: "cat output.txt; exit 7", exit: 7, truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initSourceRepo(t)
			if err := os.WriteFile(filepath.Join(source, "output.txt"), []byte(test.output), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "output.txt")
			runGit(t, source, "commit", "-m", "validation output fixture")
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "review-output"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			head, err := backend.Head(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.PullRequestHeadSHA = strings.TrimSpace(head)
			receipt, err := backend.RunReviewCommand(t.Context(), info, issue, test.command)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(receipt.Output) {
				t.Errorf("receipt output is not valid UTF-8")
			}
			encoded, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			var decoded gate.CommandResult
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Output) > limit {
				t.Errorf("transported output is %d bytes, exceeds %d", len(decoded.Output), limit)
			}
			if decoded.Output != receipt.Output || decoded.ExitCode != test.exit || decoded.Command != test.command || decoded.HeadSHA != issue.PullRequestHeadSHA || decoded.TreeSHA == "" {
				t.Errorf("transport changed validation output or command identity")
			}
			if decoded.OutputTruncated != test.truncated {
				t.Errorf("truncated = %v, want %v", decoded.OutputTruncated, test.truncated)
			}
			if test.truncated && !strings.HasPrefix(decoded.Output, "[earlier output truncated]\n") {
				t.Errorf("truncation marker missing")
			}
			if strings.HasSuffix(test.output, "end\n") && !strings.HasSuffix(decoded.Output, "end\n") {
				t.Errorf("useful command tail was discarded")
			}
			if !test.truncated && !strings.Contains(test.output, "\xff") && decoded.Output != test.output {
				t.Errorf("untruncated output changed")
			}
		})
	}
}
