package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestReleaseWorkflowAuthenticatesExactCommitProvenance(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/release.yml")
	for _, marker := range []string{
		"checks: read",
		"statuses: read",
		"commits/$GITHUB_SHA/check-runs?filter=all&per_page=100",
		"commits/$GITHUB_SHA/status?per_page=100",
		"repos/$GITHUB_REPOSITORY\" --jq '.default_branch'",
		"scripts/collect-release-rulesets.sh \"$GITHUB_REPOSITORY\" \"$rulesets\"",
		"-default-branch-ref \"$default_branch_ref\"",
		"-github-check-runs \"$check_runs\"",
		"-github-statuses \"$statuses\"",
		"-github-rulesets \"$rulesets\"",
		"DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON: ${{ vars.DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON }}",
		` . + ["scheduled-full-ci", "release-security-audit"]`,
		"-required-check-names-json \"$DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON\"",
	} {
		if !strings.Contains(workflow, marker) {
			t.Fatalf("release workflow missing authenticated provenance marker %q", marker)
		}
	}

	collector := readNormalizedFile(t, "scripts/collect-release-rulesets.sh")
	for _, marker := range []string{
		"set -euo pipefail",
		"rulesets?includes_parents=true&per_page=100",
		"> \"$ruleset_ids\"",
		"done < \"$ruleset_ids\"",
		"mv \"$rulesets_staged\" \"$output\"",
	} {
		if !strings.Contains(collector, marker) {
			t.Fatalf("release ruleset collector missing fail-closed marker %q", marker)
		}
	}
	if strings.Contains(collector, "< <(") {
		t.Fatal("release ruleset discovery must not run inside process substitution")
	}
}

func TestReleaseHooksDoNotRegenerate(t *testing.T) {
	t.Parallel()

	config := readNormalizedFile(t, ".goreleaser.yaml")
	hooks := workflowBetween(t, config, "before:", "\nbuilds:")
	for _, forbidden := range []string{"make generate", "sqlc@", "sqlc generate"} {
		t.Run(forbidden, func(t *testing.T) {
			if strings.Contains(hooks, forbidden) {
				t.Errorf("release hooks must verify committed sources, found %q", forbidden)
			}
		})
	}
}

func TestMakeTestTargetsIsolateAPIToken(t *testing.T) {
	t.Parallel()

	makefile := readNormalizedFile(t, "Makefile")
	if !strings.Contains(makefile, "GO_TEST := env -u DETENT_API_TOKEN go test") {
		t.Fatal("Makefile Go test command must remove inherited DETENT_API_TOKEN")
	}
	if got, want := strings.Count(makefile, "go test"), strings.Count(makefile, "env -u DETENT_API_TOKEN go test"); got != want {
		t.Fatalf("Makefile runs go test %d times, only %d remove inherited DETENT_API_TOKEN", got, want)
	}
}

func TestGolangCILintUsesRepositoryPinnedVersion(t *testing.T) {
	t.Parallel()

	version := strings.TrimSpace(readNormalizedFile(t, ".golangci-version"))
	if !strings.HasPrefix(version, "v2.") {
		t.Fatalf("golangci-lint version = %q, want a v2 release", version)
	}

	makefile := readNormalizedFile(t, "Makefile")
	for _, want := range []string{
		"GOLANGCI_LINT_VERSION_FILE := .golangci-version",
		"GOLANGCI_LINT_VERSION := $(shell cat $(GOLANGCI_LINT_VERSION_FILE))",
		"GOLANGCI_LINT_TOOLCHAIN := $(shell awk '/^toolchain / { print $$2 }' go.mod)",
		"GOLANGCI_LINT_DIR := $(CURDIR)/tmp/tools/golangci-lint/$(GOLANGCI_LINT_VERSION)/$(GOLANGCI_LINT_TOOLCHAIN)",
		"lint: $(GOLANGCI_LINT)",
		`GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" "$(GOLANGCI_LINT)" run --allow-parallel-runners --concurrency=$(TEST_PROCS) --timeout=15m`,
		`@bash scripts/runner-setup.sh go-tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint $(GOLANGCI_LINT_VERSION) "$(GOLANGCI_LINT)" "$(GOLANGCI_LINT_TOOLCHAIN)"`,
	} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("Makefile missing pinned golangci-lint contract %q", want)
		}
	}
	if strings.Contains(makefile, "cmd/golangci-lint@"+version) {
		t.Fatal("Makefile must read the golangci-lint version from .golangci-version")
	}
}

func TestMakeLintIgnoresAmbientBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	if runtime.GOOS == "windows" {
		t.Skip("Makefile tooling requires a POSIX shell")
	}
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Fatal(err)
	}
	for _, cached := range []bool{false, true} {
		name := "install"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, content string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "Makefile"), readNormalizedFile(t, "Makefile"))
			write(filepath.Join(root, "scripts", "runner-setup.sh"), readNormalizedFile(t, "scripts/runner-setup.sh"))
			write(filepath.Join(root, ".golangci-version"), "v2.9.0\n")
			write(filepath.Join(root, "go.mod"), "module example.com/test\n\ngo 1.26\n\ntoolchain go1.26.6\n")
			bin := filepath.Join(root, "bin")
			write(filepath.Join(bin, "golangci-lint"), "#!/bin/sh\necho incompatible ambient linter >&2\nexit 99\n")
			linter := "#!/bin/sh\nprintf 'pinned:%s:%s\\n' \"$GOTOOLCHAIN\" \"$*\"\n"
			fixture := filepath.Join(root, "pinned-linter")
			write(fixture, linter)
			write(filepath.Join(bin, "go"), "#!/bin/sh\nset -eu\n[ \"$GOTOOLCHAIN\" = go1.26.6 ]\ncase \"$*\" in 'install -p '*' github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0') ;; *) exit 1 ;; esac\ncp pinned-linter \"$GOBIN/golangci-lint\"\n")
			if cached {
				cachedPath := filepath.Join(root, "tmp/tools/golangci-lint/v2.9.0/go1.26.6/golangci-lint")
				write(cachedPath, linter)
				old := time.Unix(1, 0)
				if err := os.Chtimes(cachedPath, old, old); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), makePath, "lint")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make lint: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "pinned:go1.26.6:run --allow-parallel-runners ") || !strings.Contains(string(output), "--timeout=15m") {
				t.Fatalf("make lint did not invoke the pinned toolchain: %s", output)
			}
			if installed := strings.Contains(string(output), "Installing github.com/golangci/golangci-lint"); installed == cached {
				t.Fatalf("make lint installed = %v, cached = %v: %s", installed, cached, output)
			}
		})
	}
}

func TestDeployStagingRunsFromReleaseOnHostedRunner(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/deploy-staging.yml")
	triggers := workflowBetween(t, workflow, "on:\n", "\npermissions:")
	if want := "on:\n  workflow_call:\n    inputs:\n      version:\n        required: true\n        type: string\n"; triggers != want {
		t.Fatalf("deploy-staging triggers = %q, want exactly %q", triggers, want)
	}
	for _, test := range []struct {
		name    string
		want    string
		present bool
	}{
		{name: "hosted runner", want: "    runs-on: ubuntu-latest\n", present: true},
		{name: "staging environment", want: "    environment:\n      name: staging\n      url: https://staging.cloud.detent.build\n", present: true},
		{name: "read-only token", want: "permissions:\n  contents: read\n", present: true},
		{name: "release artifact", want: "          name: release-hub-binary\n", present: true},
		{name: "release version", want: "          RELEASE_VERSION: ${{ steps.source.outputs.version }}\n", present: true},
		{name: "develop build on push", want: "Build develop binary"},
		{name: "competing deployment concurrency", want: "concurrency:"},
		{name: "source asset preparation", want: "prepare-conversation"},
		{name: "release commit", want: "          RELEASE_COMMIT: ${{ steps.source.outputs.commit }}\n", present: true},
		{name: "shared release deployment", want: `bash scripts/deploy-release.sh staging "$RELEASE_COMMIT" "$RELEASE_VERSION"`, present: true},
		{name: "release identity smoke", want: `python3 scripts/cloud-origin-smoke.py --environment staging --expected-version "$RELEASE_VERSION" --expected-commit "$RELEASE_COMMIT"`, present: true},
		{name: "self-hosted runner", want: "self-hosted"},
		{name: "pull request trigger", want: "pull_request"},
		{name: "secrets inherited from repository", want: "secrets: inherit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := strings.Contains(workflow, test.want); got != test.present {
				t.Fatalf("deploy-staging contains %q = %t, want %t", test.want, got, test.present)
			}
		})
	}
	if count := strings.Count(workflow, "runs-on:"); count != 1 {
		t.Fatalf("deploy-staging has %d runs-on entries, want 1", count)
	}

	release := readNormalizedFile(t, ".github/workflows/release.yml")
	if !strings.Contains(release, "concurrency:\n  group: validated-release\n  cancel-in-progress: false\n") {
		t.Fatal("release must serialize the complete staging-to-production sequence without cancellation")
	}
	for _, test := range []struct {
		name, dependency, end string
	}{
		{name: "staging", dependency: "prepare", end: "\n  production:"},
		{name: "production", dependency: "[prepare, staging]"},
	} {
		t.Run(test.name+" release dependency", func(t *testing.T) {
			t.Parallel()
			job := workflowBetween(t, release, "\n  "+test.name+":\n", test.end)
			for _, want := range []string{
				"    needs: " + test.dependency + "\n",
				"    uses: ./.github/workflows/deploy-" + test.name + ".yml\n",
				"    with:\n      version: ${{ needs.prepare.outputs.version }}\n",
			} {
				if !strings.Contains(job, want) {
					t.Fatalf("release %s job missing %q", test.name, want)
				}
			}
		})
	}
	prepare := workflowBetween(t, release, "\n  prepare:\n", "\n  release:")
	for _, want := range []string{
		"    needs: source\n",
		"- name: Verify release-branch full-suite provenance\n",
		"args: build --single-target --clean\n",
		"          GOOS: linux\n",
		"          GOARCH: amd64\n",
		"          name: release-hub-binary\n",
		"          name: release-inputs\n",
		"            tmp/detent_release_provenance.json\n",
		"            tmp/prebuilt\n",
	} {
		if !strings.Contains(prepare, want) {
			t.Fatalf("Hub preparation missing %q", want)
		}
	}
	packaging := workflowBetween(t, release, "\n  release:\n", "\n  staging:")
	for _, want := range []string{
		"    needs: prepare\n",
		"          name: release-inputs\n",
		"          path: tmp\n",
		"      DETENT_PREBUILT_DIR: ${{ github.workspace }}/tmp/prebuilt\n",
	} {
		if !strings.Contains(packaging, want) {
			t.Fatalf("release packaging missing %q", want)
		}
	}
}

func TestReleaseHubArtifactMatchesPackagedBinary(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readNormalizedFile(t, ".github/workflows/release.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["prepare"].Steps {
		if step.Name == "Prepare Hub deployment artifact" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("Hub artifact must be prepared before release packaging")
	}
	for _, test := range []struct {
		name  string
		count int
	}{
		{name: "missing binary"},
		{name: "single binary", count: 1},
		{name: "ambiguous binaries", count: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for i := range test.count {
				dir := filepath.Join(root, "tmp", "goreleaser", "detent_linux_amd64_v"+strconv.Itoa(i+1))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "detent"), []byte("verified Linux binary"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.CommandContext(t.Context(), "bash", "-e", "-c", script)
			command.Dir = root
			command.Env = append(os.Environ(), "RUNNER_TEMP="+root)
			output, err := command.CombinedOutput()
			if (err != nil) != (test.count != 1) {
				t.Fatalf("prepare artifact: %v: %s", err, output)
			}
			if test.count == 1 {
				wrapper, err := filepath.Abs("scripts/goreleaser-go.sh")
				if err != nil {
					t.Fatal(err)
				}
				prebuilt := filepath.Join(root, "tmp", "prebuilt")
				if err := os.Chmod(filepath.Join(prebuilt, "linux_amd64", "detent"), 0o644); err != nil {
					t.Fatal(err)
				}
				build := exec.CommandContext(t.Context(), "bash", wrapper, "build", "-o", "packaged-detent", "./cmd/detent")
				build.Dir = root
				build.Env = append(os.Environ(), "DETENT_PREBUILT_DIR="+prebuilt, "GOOS=linux", "GOARCH=amd64")
				if output, err := build.CombinedOutput(); err != nil {
					t.Fatalf("package binary: %v: %s", err, output)
				}
				for _, artifact := range []string{"detent", "tmp/prebuilt/linux_amd64/detent", "packaged-detent"} {
					data, err := os.ReadFile(filepath.Join(root, artifact))
					if err != nil || string(data) != "verified Linux binary" {
						t.Fatalf("artifact %s = %q, %v", artifact, data, err)
					}
				}
			}
		})
	}
}

func readNormalizedFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

func workflowBetween(t *testing.T, content string, startMarker string, endMarker string) string {
	t.Helper()

	start := strings.Index(content, startMarker)
	if start == -1 {
		t.Fatalf("workflow missing marker %q", startMarker)
	}
	section := content[start:]
	if endMarker == "" {
		return section
	}
	end := strings.Index(section[len(startMarker):], endMarker)
	if end == -1 {
		t.Fatalf("workflow missing end marker %q after %q", endMarker, startMarker)
	}
	return section[:len(startMarker)+end]
}
