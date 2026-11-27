package checkpoint

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/internal/model"
)

func TestCheckpointGitReadFailures(t *testing.T) {
	t.Parallel()
	synthetic := []string{"ghp_" + "abcdefghijklmnopqrstuvwxyz123456789", "AKIA" + "1234567890123456", "AKIA" + "1234567890ABCDEF"}
	for _, scenario := range []string{"missing blob", "unreadable entry", "unreadable blob", "symlink history", "invalid entry", "unmerged index entry", "empty remote", "invalid remote", "unreadable remote", "unrelated base", "unreadable history", "unreadable diff", "empty commit", "synthetic fixtures", "altered synthetic", "altered email aws fixture", "altered slack aws fixture", "synthetic and credential", "credential before synthetic", "private key", "github token", "github fine grained token", "aws key", "project key"} {
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
					case "unmerged index entry":
						return "100644 hash 2\tfile.go\x00", nil
					}
					return "100644 blob hash\tfile.go\x00", nil
				case "cat-file":
					if scenario == "unmerged index entry" {
						t.Fatal("unmerged index entry must not call cat-file")
					}
					if len(args) != 3 || args[1] != "blob" || args[2] != "hash" {
						t.Fatalf("unexpected cat-file args %v", args)
					}
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
			case "unmerged index entry":
				err = Blob(t.Context(), git, ":", "file.go")
			default:
				err = Blob(t.Context(), git, "commit:", "file.go")
			}
			if scenario == "unmerged index entry" && !errors.Is(err, ErrUnsafe) {
				t.Fatalf("unmerged index entry error = %v, want ErrUnsafe", err)
			}
			wantOK := scenario == "missing blob" || scenario == "empty remote" || scenario == "empty commit" || scenario == "synthetic fixtures"
			if (err == nil) != wantOK {
				t.Fatalf("Git failure handling = %v", err)
			}
		})
	}
}

func TestHistoryDeduplicatesBlobsAndValidatesRawModes(t *testing.T) {
	t.Parallel()
	const blobOID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const zeroOID = "0000000000000000000000000000000000000000"
	tests := []struct {
		name         string
		commits      string
		diffByCommit map[string]string
		wantErr      error
		wantCatFiles int
	}{
		{
			name:    "deduplicates identical blob across commits",
			commits: "c1\nc2\n",
			diffByCommit: map[string]string{
				"c1": ":000000 100644 " + zeroOID + " " + blobOID + " A\x00file.go\x00",
				"c2": ":100644 100644 " + blobOID + " " + blobOID + " M\x00file.go\x00",
			},
			wantCatFiles: 1,
		},
		{
			name:    "deleted file skips blob scan",
			commits: "c1\n",
			diffByCommit: map[string]string{
				"c1": ":100644 000000 " + blobOID + " " + zeroOID + " D\x00file.go\x00",
			},
			wantCatFiles: 0,
		},
		{
			name:    "symlink mode rejected without reading blob",
			commits: "c1\n",
			diffByCommit: map[string]string{
				"c1": ":000000 120000 " + zeroOID + " " + blobOID + " A\x00file.go\x00",
			},
			wantErr:      ErrUnsafe,
			wantCatFiles: 0,
		},
		{
			name:    "malformed raw output rejected",
			commits: "c1\n",
			diffByCommit: map[string]string{
				"c1": "not-a-raw-header\x00file.go\x00",
			},
			wantErr:      ErrUnsafe,
			wantCatFiles: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			catFiles := 0
			git := func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "merge-base":
					return "", nil
				case "rev-list":
					return tt.commits, nil
				case "diff-tree":
					return tt.diffByCommit[args[len(args)-1]], nil
				case "cat-file":
					catFiles++
					return "package example\n", nil
				default:
					t.Fatalf("unexpected git command %v", args)
					return "", nil
				}
			}
			err := History(t.Context(), git, "base", "head", []string{"file.go"})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("History() error = %v, want %v", err, tt.wantErr)
			}
			if catFiles != tt.wantCatFiles {
				t.Fatalf("cat-file calls = %d, want %d", catFiles, tt.wantCatFiles)
			}
		})
	}
}
