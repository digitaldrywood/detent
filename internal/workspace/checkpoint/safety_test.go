package checkpoint

import (
	"context"
	"errors"
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
			remaining := 2 * (phaseBlobOverhead + len("ok"))
			pathsRemaining := 2
			changed := ":100644 100644 " + second + " " + first + " M\x00file.txt\x00" +
				":100644 100644 " + first + " " + second + " M\x00file.txt\x00"
			err := screenPhasePaths(git, changed, &pathsRemaining, &remaining)
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
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "commit count", want: "commit limit"},
		{name: "commit message output", want: "commit message exceeds the output limit"},
		{name: "changed paths output", want: "changed paths exceeds the output limit"},
		{name: "changed paths across commits", want: "changed path limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := func(limit int, args ...string) (string, bool, error) {
				switch args[0] {
				case "rev-list":
					if args[1] != "--max-count=2049" || limit != 2049*65 {
						t.Fatalf("rev-list bounds = %d, %v", limit, args)
					}
					if tc.name == "commit count" {
						return strings.Repeat(commit+"\n", 2049), false, nil
					}
					return strings.Repeat(commit+"\n", 2), false, nil
				case "cat-file":
					if args[1] != "commit" || limit != 1<<20 {
						t.Fatalf("unexpected commit read: %d, %v", limit, args)
					}
					return "tree example\n\nmessage\n", tc.name == "commit message output", nil
				case "diff-tree":
					if limit != 4<<20 {
						t.Fatalf("unexpected changed paths output limit: %d", limit)
					}
					path := ":100644 000000 " + commit + " " + strings.Repeat("0", 40) + " D\x00deleted.txt\x00"
					return strings.Repeat(path, 4097), tc.name == "changed paths output", nil
				}
				t.Fatalf("unexpected git command %v", args)
				return "", false, nil
			}
			err := PhaseHistory(git, commit, strings.Repeat("2", 40))
			if !errors.Is(err, ErrUnsafe) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("scan error = %v, want %q", err, tc.want)
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
