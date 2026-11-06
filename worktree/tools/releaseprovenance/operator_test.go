package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

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
		if step.Name == "Verify source belongs to its release series" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("release source ancestry check missing")
	}
	for _, test := range []struct {
		name, tag, ancestor string
		stale               bool
		wantErr             bool
	}{
		{name: "develop-only tag refused", tag: "v1.2.3", ancestor: "origin/develop", wantErr: true},
		{name: "stale release ancestor refused", tag: "v1.2.3", ancestor: "origin/release/v1.2", stale: true, wantErr: true},
		{name: "release cherry pick", tag: "v1.2.3", ancestor: "origin/release/v1.2"},
		{name: "develop dispatch refused", tag: "develop", ancestor: "origin/develop", wantErr: true},
		{name: "wrong release series", tag: "v1.3.0", ancestor: "origin/release/v1.2", wantErr: true},
		{name: "unrelated source", tag: "v1.2.3", wantErr: true},
		{name: "operator prerelease refused", tag: "v1.2.4-op.r.333.gabcdef123456", ancestor: "origin/release/v1.2", wantErr: true},
		{name: "noncanonical stable tag refused", tag: "v1.2.03", ancestor: "origin/release/v1.2", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			git := `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  fetch) ;;
  rev-parse)
    if [ "$2" = "$GITHUB_SHA^{commit}" ]; then
      printf '%s\n' "$GITHUB_SHA"
    elif [ "$2" = "$FIXTURE_ANCESTOR^{commit}" ]; then
      if [ "$FIXTURE_STALE" = true ]; then printf 'newer-head\n'; else printf '%s\n' "$GITHUB_SHA"; fi
    else exit 1; fi ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "bash", "-e", "-c", script)
			command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "GITHUB_SHA="+strings.Repeat("a", 40), "GITHUB_REF_NAME="+test.tag, "FIXTURE_ANCESTOR="+test.ancestor, "FIXTURE_STALE="+map[bool]string{true: "true", false: "false"}[test.stale])
			output, err := command.CombinedOutput()
			if (err != nil) != test.wantErr {
				t.Fatalf("release source = %v: %s", err, output)
			}
		})
	}
}
