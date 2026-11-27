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
	head := strings.Repeat("1", 40)
	tree := strings.Repeat("2", 40)
	subtree := strings.Repeat("3", 40)
	blob := strings.Repeat("4", 40)
	base := strings.Repeat("5", 40)
	parent := strings.Repeat("6", 40)
	parentTree := strings.Repeat("7", 40)

	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "commit count", want: "commit limit"},
		{name: "large commit", want: "commit object exceeds the size limit"},
		{name: "large tree", want: "tree object exceeds the size limit"},
		{name: "incorrect tree type", want: "invalid phase tree object type"},
		{name: "tree entries output", want: "tree entries exceeds the output limit"},
		{name: "parent edges across commits", want: "parent edge limit"},
		{name: "changed paths across commits", want: "changed path limit"},
		{name: "reused subtrees exhaust the budget during traversal", want: "changed path limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sharedTreeReads := 0
			git := func(_ int, args ...string) (string, bool, error) {
				switch args[0] {
				case "rev-list":
					if !slices.Equal(args[2:], []string{base}) {
						t.Fatalf("commit traversal received unpublished input: %v", args)
					}
					return base + "\n", false, nil
				case "cat-file":
					oid := args[2]
					switch args[1] {
					case "-s":
						if tc.name == "large commit" && oid == head {
							return "1048577\n", false, nil
						}
						if tc.name == "large tree" && oid == tree {
							return "4194305\n", false, nil
						}
						return "128\n", false, nil
					case "-t":
						if oid == tree || oid == subtree || oid == parentTree {
							if tc.name == "incorrect tree type" {
								return "tag\n", false, nil
							}
							return "tree\n", false, nil
						}
						return "commit\n", false, nil
					case "commit":
						if tc.name == "large commit" || tc.name == "parent edges across commits" && oid == base {
							t.Fatalf("unexpected commit content read for %s", oid)
						}
						var parents string
						switch tc.name {
						case "commit count":
							next := 1
							if oid != head {
								if _, err := fmt.Sscanf(oid, "%x", &next); err != nil {
									t.Fatal(err)
								}
								next++
							}
							parents = fmt.Sprintf("parent %040x\n", next)
						case "parent edges across commits":
							parents = strings.Repeat("parent "+base+"\n", 2048)
							if oid == head {
								parents += "parent " + parent + "\n"
							}
						case "changed paths across commits":
							if oid == head {
								return "tree " + tree + "\nparent " + parent + "\n\nmessage\n", false, nil
							}
							return "tree " + parentTree + "\n\nmessage\n", false, nil
						}
						return "tree " + tree + "\n" + parents + "\nmessage\n", false, nil
					}
				case "ls-tree":
					if len(args) != 3 || args[1] != "-z" {
						t.Fatalf("expected a non-recursive tree read, got %v", args)
					}
					var entries strings.Builder
					switch tc.name {
					case "tree entries output":
						return "", true, nil
					case "reused subtrees exhaust the budget during traversal":
						if args[2] == subtree {
							sharedTreeReads++
							for i := range 100 {
								fmt.Fprintf(&entries, "100644 blob %s\tfile-%03d\x00", blob, i)
							}
						} else {
							for i := range 100 {
								fmt.Fprintf(&entries, "040000 tree %s\tdir-%03d\x00", subtree, i)
							}
						}
					case "changed paths across commits":
						switch args[2] {
						case tree:
							for i := range 2731 {
								fmt.Fprintf(&entries, "040000 tree %s\tdir-%04d\x00", subtree, i)
							}
						case parentTree:
							for i := range 2731 {
								fmt.Fprintf(&entries, "040000 tree %s\tother-%04d\x00", subtree, i)
							}
						}
					default:
						t.Fatalf("unexpected tree content read for %s", args[2])
					}
					return entries.String(), false, nil
				}
				t.Fatalf("unexpected git command %v", args)
				return "", false, nil
			}
			err := PhaseHistory(git, head, base)
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
					return parent + "\n" + firstParent + "\n", false, nil
				case "cat-file":
					switch args[1] {
					case "-s":
						return "128\n", false, nil
					case "-t":
						if args[2] == before || args[2] == after {
							return "tree\n", false, nil
						}
						return "commit\n", false, nil
					}
					switch args[2] {
					case parent:
						return "tree " + before + "\n\nmessage\n", false, nil
					case firstParent:
						return "tree " + after + "\n\nmessage\n", false, nil
					}
					parents := "parent " + parent + "\n"
					if tc.merge {
						parents = "parent " + firstParent + "\n" + parents
					}
					return "tree " + after + "\n" + parents + "author " + tc.author + " <test@example.test> 1 +0000\n\nmessage\n", false, nil
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
			err := PhaseHistory(git, commit, firstParent)
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
