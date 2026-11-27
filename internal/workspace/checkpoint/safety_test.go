package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/internal/model"
)

func TestPhaseHistoryStopsBeforeReadingOverBudgetBlob(t *testing.T) {
	first, second := strings.Repeat("1", 40), strings.Repeat("2", 40)
	for _, scenario := range []struct {
		name       string
		secondSize string
		wantError  string
		wantReads  []string
	}{
		{name: "at aggregate limit", secondSize: "2", wantReads: []string{first, second}},
		{name: "over aggregate limit", secondSize: "3", wantError: "aggregate scan limit", wantReads: []string{first}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var reads []string
			git := func(_ int, args ...string) (string, bool, error) {
				switch args[1] {
				case "-s":
					if args[2] == second {
						return scenario.secondSize + "\n", false, nil
					}
					return "2\n", false, nil
				case "blob":
					reads = append(reads, args[2])
					return "ok", false, nil
				}
				t.Fatalf("unexpected git command %v", args)
				return "", false, nil
			}
			scan := phaseScan{git: git, remaining: 2 * (phaseBlobOverhead + len("ok")), blobs: []string{first, second}}
			err := scan.screenBlobs()
			if scenario.wantError == "" && err != nil || scenario.wantError != "" && (!errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), scenario.wantError)) {
				t.Fatalf("scan error = %v, want %q", err, scenario.wantError)
			}
			if !slices.Equal(reads, scenario.wantReads) {
				t.Fatalf("read blobs %v, want %v", reads, scenario.wantReads)
			}
		})
	}
}

func TestPhaseHistoryBoundsMetadata(t *testing.T) {
	commit := strings.Repeat("1", 40)
	tree := strings.Repeat("2", 40)
	subtree := strings.Repeat("3", 40)
	blob := strings.Repeat("4", 40)
	previousCommit := strings.Repeat("5", 40)
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "commit count", want: "commit limit"},
		{name: "commit object output", want: "commit object exceeds the output limit"},
		{name: "tree entries output", want: "tree entries exceeds the output limit"},
		{name: "changed paths across commits", want: "changed path limit"},
		{name: "reused subtrees exhaust the budget during traversal", want: "changed path limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sharedTreeReads := 0
			git := func(limit int, args ...string) (string, bool, error) {
				switch args[0] {
				case "rev-list":
					if args[1] != "--max-count=2049" || limit != 2049*65 {
						t.Fatalf("rev-list bounds = %d, %v", limit, args)
					}
					if tc.name == "commit count" {
						return strings.Repeat(commit+"\n", 2049), false, nil
					}
					if tc.name == "reused subtrees exhaust the budget during traversal" {
						return commit + "\n", false, nil
					}
					return commit + "\n" + previousCommit + "\n", false, nil
				case "cat-file":
					if args[1] != "commit" || limit != 1<<20 {
						t.Fatalf("unexpected commit read: %d, %v", limit, args)
					}
					return "tree " + tree + "\n\nmessage\n", tc.name == "commit object output", nil
				case "ls-tree":
					if limit != 4<<20 || len(args) != 3 || args[1] != "-z" {
						t.Fatalf("unexpected single-tree read: %d, %v", limit, args)
					}
					if tc.name == "reused subtrees exhaust the budget during traversal" {
						var entries strings.Builder
						switch args[len(args)-1] {
						case tree:
							for i := range 100 {
								fmt.Fprintf(&entries, "040000 tree %s\tdir-%03d\x00", subtree, i)
							}
						case subtree:
							sharedTreeReads++
							for i := range 100 {
								fmt.Fprintf(&entries, "100644 blob %s\tfile-%03d\x00", blob, i)
							}
						}
						return entries.String(), false, nil
					}
					if tc.name == "tree entries output" {
						return "", true, nil
					}
					if args[2] == subtree {
						return "", false, nil
					}
					var entries strings.Builder
					for i := range 4097 {
						fmt.Fprintf(&entries, "040000 tree %s\tdir-%04d\x00", subtree, i)
					}
					return entries.String(), false, nil
				}
				t.Fatalf("unexpected git command %v", args)
				return "", false, nil
			}
			err := PhaseHistory(git, commit, strings.Repeat("2", 40))
			if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("scan error = %v, want %q", err, tc.want)
			}
			if tc.name == "reused subtrees exhaust the budget during traversal" && (sharedTreeReads == 0 || sharedTreeReads >= 100) {
				t.Fatalf("read %d reused trees, want traversal to stop within the path budget", sharedTreeReads)
			}
		})
	}
}

func TestPhaseHistoryScreensPublicationMetadata(t *testing.T) {
	commit := strings.Repeat("1", 40)
	parent := strings.Repeat("2", 40)
	firstParent := strings.Repeat("5", 40)
	before := strings.Repeat("3", 40)
	after := strings.Repeat("4", 40)
	credential := "ghp_" + strings.Repeat("Z", 36)
	for _, tc := range []struct {
		name   string
		author string
		path   string
		merge  bool
		unsafe bool
	}{
		{name: "ordinary metadata", author: "Test", path: "file.txt"},
		{name: "credential in commit author", author: credential, path: "file.txt", unsafe: true},
		{name: "credential in deleted path", author: "Test", path: credential + ".txt", unsafe: true},
		{name: "credential in path deleted from the second parent", author: "Test", path: credential + ".txt", merge: true, unsafe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := func(_ int, args ...string) (string, bool, error) {
				switch args[0] {
				case "rev-list":
					return commit + "\n", false, nil
				case "cat-file":
					parents := "parent " + parent + "\n"
					if tc.merge {
						parents = "parent " + firstParent + "\n" + parents
					}
					return "tree " + after + "\n" + parents + "author " + tc.author + " <test@example.test> 1 +0000\n\nmessage\n", false, nil
				case "rev-parse":
					switch args[2] {
					case firstParent + "^{tree}":
						return after + "\n", false, nil
					case parent + "^{tree}":
						return before + "\n", false, nil
					}
				case "ls-tree":
					if args[2] == before {
						return "100644 blob " + commit + "\t" + tc.path + "\x00", false, nil
					}
					if args[2] == after {
						return "", false, nil
					}
				}
				t.Fatalf("unexpected git command %v", args)
				return "", false, nil
			}
			err := PhaseHistory(git, commit)
			if tc.unsafe {
				if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), "possible credential") || strings.Contains(err.Error(), credential) {
					t.Fatalf("expected a redacted credential refusal")
				}
			} else if err != nil {
				t.Fatalf("scan ordinary metadata: %v", err)
			}
		})
	}
}

func TestCheckpointGitReadFailures(t *testing.T) {
	t.Parallel()
	synthetic := []string{"ghp_" + "abcdefghijklmnopqrstuvwxyz123456789", "AKIA" + "1234567890123456", "AKIA" + "1234567890ABCDEF"}
	for _, scenario := range []string{"missing blob", "unreadable entry", "unreadable blob", "symlink history", "invalid entry", "empty remote", "invalid remote", "unreadable remote", "unrelated base", "unreadable history", "unreadable diff", "empty commit", "synthetic fixtures", "altered synthetic", "altered email aws fixture", "altered slack aws fixture", "synthetic and credential", "credential before synthetic", "private key", "github token", "github fine grained token", "aws key", "project key"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			failure := errors.New("git unavailable")
			git := func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "ls-tree", "ls-files":
					switch scenario {
					case "missing blob":
						return "", nil
					case "unreadable entry":
						return "", failure
					case "symlink history":
						return "120000 blob hash\tfile.go\x00", nil
					case "invalid entry":
						return "invalid", nil
					}
					return "100644 blob hash\tfile.go\x00", nil
				case "show":
					switch scenario {
					case "synthetic fixtures":
						return strings.Join(synthetic, "\n"), nil
					case "altered synthetic":
						return synthetic[0] + "Z", nil
					case "altered email aws fixture":
						return synthetic[1][:len(synthetic[1])-1] + "Z", nil
					case "altered slack aws fixture":
						return synthetic[2][:len(synthetic[2])-1] + "Z", nil
					case "synthetic and credential":
						return strings.Join(synthetic, "\n") + "\nghp_" + strings.Repeat("Z", 36), nil
					case "credential before synthetic":
						return "ghp_" + strings.Repeat("Z", 36) + "\n" + strings.Join(synthetic, "\n"), nil
					case "private key":
						return "-----BEGIN " + "PRIVATE KEY-----", nil
					case "github token":
						return "ghp_" + strings.Repeat("Z", 36), nil
					case "github fine grained token":
						return "github_pat_" + strings.Repeat("Z", 36), nil
					case "aws key":
						return "AKIA" + strings.Repeat("Z", 16), nil
					case "project key":
						return "sk-proj-" + strings.Repeat("Z", 36), nil
					}
					return "", failure
				case "ls-remote":
					if scenario == "empty remote" {
						return "", nil
					}
					if scenario == "invalid remote" {
						return "unexpected ref", nil
					}
					return "", failure
				case "merge-base":
					if scenario == "unrelated base" {
						return "", failure
					}
					return "", nil
				case "rev-list":
					if scenario == "unreadable history" {
						return "", failure
					}
					return "commit", nil
				case "diff-tree":
					if scenario == "unreadable diff" {
						return "", failure
					}
					return "", nil
				}
				t.Fatalf("unexpected git command %v", args)
				return "", failure
			}
			var err error
			switch scenario {
			case "empty remote", "invalid remote", "unreadable remote":
				_, err = RemoteHead(t.Context(), git, Plan{Info: model.Info{Branch: "owned"}, RemoteURL: "remote"})
			case "unrelated base", "unreadable history", "unreadable diff", "empty commit":
				err = History(t.Context(), git, "base", "head", []string{"file.go"})
			default:
				err = Blob(t.Context(), git, "commit:", "file.go")
			}
			wantOK := scenario == "missing blob" || scenario == "empty remote" || scenario == "empty commit" || scenario == "synthetic fixtures"
			if (err == nil) != wantOK {
				t.Fatalf("Git failure handling = %v", err)
			}
		})
	}
}
