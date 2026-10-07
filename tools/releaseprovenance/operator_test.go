package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
)

func TestOperatorReleaseTagBindsVerifiedSource(t *testing.T) {
	const commit = "abcdef1234567890abcdef1234567890abcdef12"
	for _, test := range []struct {
		name, latest, checkCommit, conclusion, app, existingCommit string
		selectedID                                                 int
		wantErr                                                    bool
	}{
		{name: "next patch", latest: "v0.117.47"},
		{name: "next patch after stable", latest: "v0.117.48"},
		{name: "not a stable release", latest: "v0.117.48-op.abcdef123456", wantErr: true},
		{name: "noncanonical stable version", latest: "v0.117.047", wantErr: true},
		{name: "wrong source", checkCommit: strings.Repeat("b", 40), wantErr: true},
		{name: "failed source gate", conclusion: "failure", wantErr: true},
		{name: "untrusted check app", app: "other", wantErr: true},
		{name: "different check run", selectedID: 102, wantErr: true},
		{name: "tag collision", existingCommit: strings.Repeat("b", 40), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			latest := test.latest
			if latest == "" {
				latest = "v0.117.47"
			}
			checkCommit := test.checkCommit
			if checkCommit == "" {
				checkCommit = commit
			}
			conclusion := test.conclusion
			if conclusion == "" {
				conclusion = "success"
			}
			app := test.app
			if app == "" {
				app = "github-actions"
			}
			selectedID := test.selectedID
			if selectedID == 0 {
				selectedID = 101
			}
			checks := fmt.Sprintf(`{"total_count":1,"check_runs":[{"id":101,"name":"Verify operator-landed source","head_sha":%q,"status":"completed","conclusion":%q,"app":{"id":15368,"slug":%q}}]}`, checkCommit, conclusion, app)
			checksPath := filepath.Join(dir, "checks.json")
			if err := os.WriteFile(checksPath, []byte(checks), 0o600); err != nil {
				t.Fatal(err)
			}
			git := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  rev-parse)
    if [ "$2" = HEAD ]; then printf '%s\n' "$FIXTURE_COMMIT"; else printf '%s\n' "$FIXTURE_EXISTING_COMMIT"; fi ;;
  merge-base) test "$2" = --is-ancestor && test "$3" = "$FIXTURE_COMMIT" && test "$4" = origin/develop ;;
  show-ref) test -n "$FIXTURE_EXISTING_COMMIT" ;;
  -c) test "$5" = tag && test "$6" = -a && test "$8" = "$FIXTURE_COMMIT"; cp "${10}" "$FIXTURE_MESSAGE" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			messagePath := filepath.Join(dir, "message")
			command := exec.CommandContext(t.Context(), "bash", "../../scripts/prepare-operator-release.sh", commit, latest, checksPath, strconv.Itoa(selectedID))
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+dir, "GITHUB_REPOSITORY=digitaldrywood/detent", "FIXTURE_COMMIT="+commit, "FIXTURE_EXISTING_COMMIT="+test.existingCommit, "FIXTURE_MESSAGE="+messagePath)
			output, err := command.CombinedOutput()
			if (err != nil) != test.wantErr {
				t.Fatalf("operator tag = %v: %s", err, output)
			}
			if test.wantErr {
				if _, err := os.Stat(messagePath); !os.IsNotExist(err) {
					t.Fatal("unverified source created a tag")
				}
				return
			}
			wantTag := "v0.117.48-op.abcdef123456"
			if latest == "v0.117.48" {
				wantTag = "v0.117.49-op.abcdef123456"
			}
			if strings.TrimSpace(string(output)) != wantTag {
				t.Fatalf("tag = %q, want %q", output, wantTag)
			}
			message, err := os.ReadFile(messagePath)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := provenance.FromTagMessage(string(message), "digitaldrywood/detent", wantTag, commit)
			if err != nil || len(manifest.Checks) != 1 || manifest.Checks[0].CheckRunID != 101 {
				t.Fatalf("provenance = %+v, %v", manifest, err)
			}
		})
	}
}
