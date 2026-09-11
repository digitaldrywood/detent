package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCombinedCoveragePublishesOnlyCurrentSuccessfulRun(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("local Makefile gate requires Bash")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	merger := filepath.Join(t.TempDir(), "covermerge")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", merger, "./tools/covermerge")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build merger: %v: %s", err, output)
	}
	script, err := os.ReadFile("scripts/test-race-cover.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"pass", "hub-fail", "rest-fail", "list-fail", "missing-hub", "overlap", "changed-selection", "rest-race-fail", "orchestrator-fail"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			files := map[string]string{
				"gate.sh": string(script),
				"go": `#!/usr/bin/env bash
set -eu
if [ "$1" = list ]; then
    [ "$FIXTURE_MODE" != list-fail ] || exit 1
    if [ "$2" = ./... ]; then
        printf '%s\n' github.com/digitaldrywood/detent/internal/hubserver github.com/digitaldrywood/detent/internal/orchestrator example.com/rest
    elif [ "$FIXTURE_MODE" = changed-selection ] && [ "$2" = -race ]; then
        printf '%s\n' changed.go
    else
        printf '%s\n' current.go
    fi
elif [ "$1 $2" = 'run ./tools/covermerge' ]; then
    shift 2
    exec "$FIXTURE_MERGER" "$@"
else
    [ -z "${DETENT_API_TOKEN:-}" ] || exit 9
    if [ "$1" = run ] && [ "${!#}" = ./internal/orchestrator ]; then
        [ "$FIXTURE_MODE" != orchestrator-fail ]
        exit
    fi
    group=rest
    if [ "$1" = run ]; then group=hub; fi
    profile_mode=set
    if [ "$group" = hub ]; then profile_mode=atomic; fi
    if [ "$1 $2" = 'test -race' ]; then
        for argument in "$@"; do
            [ "$argument" != github.com/digitaldrywood/detent/internal/orchestrator ] || exit 7
        done
        [ "$FIXTURE_MODE" != rest-race-fail ]
        exit
    fi
    profile=
    while [ "$#" -gt 0 ]; do
        case "$1" in
            -coverprofile) shift; profile="$1" ;;
            -coverprofile=*) profile="${1#-coverprofile=}" ;;
        esac
        shift
    done
    [ -n "$profile" ] || exit 8
    if [ "$FIXTURE_MODE" = missing-hub ] && [ "$group" = hub ]; then exit 0; fi
    if [ "$FIXTURE_MODE" = overlap ]; then group=hub; fi
    printf 'mode: %s\nexample.com/%s/file%s.go:1.1,2.2 1 1\n' "$profile_mode" "$group" "$FIXTURE_INPUT" > "$profile"
    [ "$FIXTURE_MODE" != "$group-fail" ] || exit 1
fi
`,
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			outputPath := filepath.Join(dir, "published.out")
			if err := os.WriteFile(outputPath, []byte("stale prior result"), 0o600); err != nil {
				t.Fatal(err)
			}
			orphan := filepath.Join(dir, "detent-race-cover.orphan")
			if err := os.Mkdir(orphan, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(orphan, "hub.out"), []byte("stale interrupted result"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, input := range []string{"1", "2"} {
				command := exec.CommandContext(t.Context(), bash, "gate.sh", "2", "15m", outputPath, "4", "20m")
				command.Dir = dir
				command.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+dir, "DETENT_API_TOKEN=fixture-token", "FIXTURE_MODE="+mode, "FIXTURE_INPUT="+input, "FIXTURE_MERGER="+merger)
				output, err := command.CombinedOutput()
				if (err == nil) != (mode == "pass") {
					t.Fatalf("gate error = %v: %s", err, output)
				}
				profile, err := os.ReadFile(outputPath)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "pass" {
					if strings.Contains(string(profile), "stale") || strings.Count(string(profile), "/file"+input+".go:") != 2 {
						t.Fatalf("published wrong input: %s", profile)
					}
				} else if string(profile) != "stale prior result" {
					t.Fatalf("failed gate published partial result: %s", profile)
				}
			}
		})
	}
}
