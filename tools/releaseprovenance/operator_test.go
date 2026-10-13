package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
	"github.com/digitaldrywood/detent/internal/update"
)

func TestOperatorReleaseTagBindsVerifiedSource(t *testing.T) {
	const defaultCommit = "abcdef1234567890abcdef1234567890abcdef12"
	for _, test := range []struct {
		name, latest, commit, checkCommit, conclusion, app, existingCommit string
		runNumber, previous, priorCommit, priorRun, tagType, existingTag   string
		selectedID                                                         int
		wantErr                                                            bool
	}{
		{name: "next patch", latest: "v0.117.47"},
		{name: "next patch after stable", latest: "v0.117.48"},
		{name: "legacy descending hash transition", latest: "v0.121.3", commit: "7fb07150e1d4" + strings.Repeat("a", 28), previous: "v0.121.4-op.gbf69b66512c6"},
		{name: "next ordered release with descending hash", latest: "v0.121.3", commit: "7fb07150e1d4" + strings.Repeat("a", 28), runNumber: "10", priorCommit: "bf69b66512c6" + strings.Repeat("b", 28), priorRun: "9"},
		{name: "not a stable release", latest: "v0.117.48-op.gabcdef123456", wantErr: true},
		{name: "noncanonical stable version", latest: "v0.117.047", wantErr: true},
		{name: "noncanonical run number", runNumber: "0123", wantErr: true},
		{name: "invalid run number", runNumber: "other", wantErr: true},
		{name: "wrong source", checkCommit: strings.Repeat("b", 40), wantErr: true},
		{name: "failed source gate", conclusion: "failure", wantErr: true},
		{name: "untrusted check app", app: "other", wantErr: true},
		{name: "different check run", selectedID: 102, wantErr: true},
		{name: "tag collision", existingCommit: strings.Repeat("b", 40), wantErr: true},
		{name: "run collision with different source hash", existingCommit: strings.Repeat("b", 40), existingTag: "v0.117.48-op.r.123.gbbbbbbbbbbbb", wantErr: true},
		{name: "lightweight tag", existingCommit: defaultCommit, tagType: "commit", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			latest := test.latest
			if latest == "" {
				latest = "v0.117.47"
			}
			commit := test.commit
			if commit == "" {
				commit = defaultCommit
			}
			runNumber := test.runNumber
			if runNumber == "" {
				runNumber = "123"
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
			checksPath := filepath.Join(dir, "checks.json")
			git := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  rev-parse)
    if [ "$2" = HEAD ]; then printf '%s\n' "$FIXTURE_COMMIT"; else cat "$FIXTURE_DIR/commit"; fi ;;
  merge-base) test "$2" = --is-ancestor && test "$3" = "$FIXTURE_COMMIT" && test "$4" = origin/develop ;;
  tag)
    test "$2" = --list
    if [ -f "$FIXTURE_DIR/tag" ]; then
      existing="$(cat "$FIXTURE_DIR/tag")"
      if [[ "$existing" == $3 ]]; then printf '%s\n' "$existing"; fi
    fi ;;
  show-ref) test -f "$FIXTURE_DIR/tag" && test "$4" = "refs/tags/$(cat "$FIXTURE_DIR/tag")" ;;
  cat-file) printf '%s\n' "$FIXTURE_TAG_TYPE" ;;
  -c)
    test "$5" = tag && test "$6" = -a && test "$8" = "$FIXTURE_COMMIT"
    printf '%s\n' "$7" > "$FIXTURE_DIR/tag"
    printf '%s\n' "$8" > "$FIXTURE_DIR/commit"
    cp "${10}" "$FIXTURE_DIR/message" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			wantTag := "v0.117.48-op.r." + runNumber + ".g" + commit[:12]
			switch latest {
			case "v0.117.48":
				wantTag = "v0.117.49-op.r." + runNumber + ".g" + commit[:12]
			case "v0.121.3":
				wantTag = "v0.121.4-op.r." + runNumber + ".g" + commit[:12]
			}
			if test.existingCommit != "" {
				existingTag := test.existingTag
				if existingTag == "" {
					existingTag = wantTag
				}
				for name, value := range map[string]string{"tag": existingTag, "commit": test.existingCommit} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			tagType := test.tagType
			if tagType == "" {
				tagType = "tag"
			}
			generate := func(source, stable, number string) (string, error) {
				checkCommit := source
				if test.checkCommit != "" {
					checkCommit = test.checkCommit
				}
				checks := fmt.Sprintf(`{"total_count":1,"check_runs":[{"id":101,"name":"Verify operator-landed source","head_sha":%q,"status":"completed","conclusion":%q,"app":{"id":15368,"slug":%q}}]}`, checkCommit, conclusion, app)
				if err := os.WriteFile(checksPath, []byte(checks), 0o600); err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(t.Context(), "bash", "../../scripts/prepare-operator-release.sh", source, stable, checksPath, strconv.Itoa(selectedID), number)
				command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+dir, "GITHUB_REPOSITORY=example-org/example-project", "FIXTURE_COMMIT="+source, "FIXTURE_DIR="+dir, "FIXTURE_TAG_TYPE="+tagType)
				output, err := command.CombinedOutput()
				return strings.TrimSpace(string(output)), err
			}
			previous := test.previous
			if test.priorCommit != "" {
				var err error
				previous, err = generate(test.priorCommit, latest, test.priorRun)
				if err != nil {
					t.Fatalf("prior operator tag = %v: %s", err, previous)
				}
			}
			output, err := generate(commit, latest, runNumber)
			if (err != nil) != test.wantErr {
				t.Fatalf("operator tag = %v: %s", err, output)
			}
			messagePath := filepath.Join(dir, "message")
			if test.wantErr {
				if _, err := os.Stat(messagePath); !os.IsNotExist(err) {
					t.Fatal("unverified source created a tag")
				}
				return
			}
			if output != wantTag {
				t.Fatalf("tag = %q, want %q", output, wantTag)
			}
			if previous != "" {
				comparison, err := update.CompareVersions(output, previous)
				if err != nil || comparison != 1 {
					t.Fatalf("generated tag %q must advance %q: %d, %v", output, previous, comparison, err)
				}
			}
			stable := strings.Split(output, "-")[0]
			if comparison, err := update.CompareVersions(stable, output); err != nil || comparison != 1 {
				t.Fatalf("stable release %q must supersede %q: %d, %v", stable, output, comparison, err)
			}
			message, err := os.ReadFile(messagePath)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := provenance.FromTagMessage(string(message), "example-org/example-project", output, commit)
			if err != nil || len(manifest.Checks) != 1 || manifest.Checks[0].CheckRunID != 101 {
				t.Fatalf("provenance = %+v, %v", manifest, err)
			}
			for _, retry := range []struct {
				latest  string
				refused bool
			}{
				{latest: latest},
				{latest: stable, refused: true},
				{latest: "v0.121.99", refused: true},
				{latest: "v0.122.0", refused: true},
				{latest: "v1.0.0", refused: true},
				{latest: "v9.9.9", refused: true},
			} {
				replayed, err := generate(commit, retry.latest, runNumber)
				if (err != nil) != retry.refused || !retry.refused && replayed != output {
					t.Fatalf("replay with stable %s = %q, %v, refused=%t", retry.latest, replayed, err, retry.refused)
				}
				replayedMessage, err := os.ReadFile(messagePath)
				if err != nil || string(replayedMessage) != string(message) {
					t.Fatalf("replay replaced immutable provenance: %v", err)
				}
			}
		})
	}
}

func TestReleaseSourceHistory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["source"].Steps {
		if step.Name == "Verify source belongs to develop or its release series" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("release source ancestry check missing")
	}
	for _, test := range []struct {
		name, tag, ancestor string
		wantErr             bool
	}{
		{name: "historical develop tag", tag: "v1.2.3", ancestor: "origin/develop"},
		{name: "release cherry pick", tag: "v1.2.3", ancestor: "origin/release/v1.2"},
		{name: "operator develop source", tag: "develop", ancestor: "origin/develop"},
		{name: "wrong release series", tag: "v1.3.0", ancestor: "origin/release/v1.2", wantErr: true},
		{name: "unrelated source", tag: "v1.2.3", wantErr: true},
		{name: "operator must belong to develop", tag: "develop", ancestor: "origin/release/v1.2", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			git := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  fetch) ;;
  merge-base) test "$3" = "$GITHUB_SHA" && test "$4" = "$FIXTURE_ANCESTOR" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", "-e", "-c", script)
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_SHA="+strings.Repeat("a", 40), "GITHUB_REF_NAME="+test.tag, "FIXTURE_ANCESTOR="+test.ancestor)
			output, err := command.CombinedOutput()
			if (err != nil) != test.wantErr {
				t.Fatalf("release source = %v: %s", err, output)
			}
		})
	}
}
