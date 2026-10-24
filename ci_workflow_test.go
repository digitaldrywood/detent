package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCIReleaseAuditStatusGate(t *testing.T) {
	t.Parallel()
	var workflow struct {
		Jobs map[string]struct {
			Needs yaml.Node `yaml:"needs"`
			Steps []struct {
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readNormalizedFile(t, ".github/workflows/ci.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	if _, exists := workflow.Jobs["release-security-audit"]; exists {
		t.Fatal("release audit must run locally")
	}
	finalize := workflow.Jobs["finalize"]
	var needs []string
	if err := finalize.Needs.Decode(&needs); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(needs, "release-security-audit") {
		t.Fatal("finalizer depends on retired audit job")
	}
	for _, step := range finalize.Steps {
		if strings.Contains(step.Run, "scripts/scheduled-ci-finish.sh") {
			if step.Env["DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON"] != "${{ vars.DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON }}" {
				t.Fatal("finalizer must receive required status names from repository variables")
			}
			return
		}
	}
	t.Fatal("finalizer invocation missing")
}

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
		"must be configured as a JSON array; use [] when there are no release-only checks",
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

	workflow := workflowBetween(t, readNormalizedFile(t, ".github/workflows/ci.yml"), "  lint:", "\n  verify-fast:")
	for _, want := range []string{
		"path: tmp/tools/golangci-lint",
		"hashFiles('.golangci-version')",
		"check_with_evidence lint make lint",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("CI lint job missing pinned golangci-lint contract %q", want)
		}
	}
	for _, forbidden := range []string{"~/go/bin/golangci-lint", "cmd/golangci-lint@"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("CI lint job bypasses the Makefile pin with %q", forbidden)
		}
	}
}

func TestCIWindowsChecksUseExistingLinuxVetStep(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	triggers := workflowBetween(t, workflow, "on:\n", "\npermissions:")
	if !strings.Contains(triggers, "\n  pull_request:\n") {
		t.Fatal("Windows checks must run on pull requests")
	}
	job := workflowBetween(t, workflow, "\n  verify-fast:\n", "\n  generated:")
	if !strings.Contains(job, "    runs-on: ubuntu-latest\n") {
		t.Fatal("Windows checks must reuse the existing Linux verify-fast job")
	}
	for _, want := range []string{
		"    if: ${{ !cancelled() && (github.event_name == 'pull_request' || needs.preflight.outputs.should_run == 'true') }}\n",
		"          ref: ${{ github.event_name == 'pull_request' && github.sha || needs.preflight.outputs.develop_sha }}\n",
	} {
		if !strings.Contains(job, want) {
			t.Fatalf("Windows checks must run on the PR merge commit even when scheduled preflight is skipped: missing %q", want)
		}
	}
	for _, name := range []string{"Build", "Audit all Go packages with NilAway and the reviewed baseline", "Verify landing gate concurrency and failure propagation"} {
		if !strings.Contains(job, "      - name: "+name+"\n        if: github.event_name != 'pull_request'\n") {
			t.Errorf("%s must stay outside the PR guard", name)
		}
	}
	var parsed struct {
		Jobs map[string]struct {
			Steps []struct {
				If  string `yaml:"if"`
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &parsed); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	goRecorder := `#!/bin/sh
if [ "$1" = env ]; then exit 0; fi
operation=$1
shift
for target do
    case "$target" in
        -*) ;;
        [0-9]*) ;;
        *) printf '%s %s %s %s\n' "$GOOS" "$GOARCH" "$operation" "$target" >> "$CI_GO_CALLS" ;;
    esac
done
`
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(goRecorder), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request", "schedule", "workflow_dispatch"} {
		t.Run(event, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "go-calls")
			for _, step := range parsed.Jobs["verify-fast"].Steps {
				if step.Run == "" || step.If != "" {
					continue
				}
				cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c", step.Run)
				cmd.Env = append(os.Environ(),
					"GITHUB_EVENT_NAME="+event,
					"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),
					"GOOS=linux", "GOARCH=amd64", "CI_GO_CALLS="+calls,
				)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("workflow script: %v\n%s", err, output)
				}
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSpace(string(data)), "\n")
			want := []string{"linux amd64 vet ./..."}
			if event == "pull_request" {
				want = []string{"windows amd64 build ./...", "windows amd64 vet ./...", "windows arm64 build ./..."}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("Go operations = %q, want %q", got, want)
			}
		})
	}
	workflows, err := filepath.Glob(".github/workflows/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range workflows {
		if filepath.Base(path) == "windows-tests.yml" {
			continue
		}
		if strings.Contains(readNormalizedFile(t, path), "windows-latest") {
			t.Errorf("%s must not add a Windows runner outside the scheduled Windows workflow", path)
		}
	}
}

func TestCIScheduledWindowsTestsSeparateFromReleasePromotion(t *testing.T) {
	t.Parallel()

	var workflow struct {
		On   map[string]yaml.Node `yaml:"on"`
		Jobs map[string]struct {
			If     string    `yaml:"if"`
			Needs  yaml.Node `yaml:"needs"`
			RunsOn string    `yaml:"runs-on"`
			Steps  []struct {
				Uses string            `yaml:"uses"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readNormalizedFile(t, ".github/workflows/windows-tests.yml")), &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 2 {
		t.Fatalf("Windows workflow triggers = %v, want schedule and workflow_dispatch only", workflow.On)
	}
	for _, event := range []string{"schedule", "workflow_dispatch"} {
		if _, ok := workflow.On[event]; !ok {
			t.Errorf("Windows workflow missing %s trigger", event)
		}
	}
	job, ok := workflow.Jobs["windows-go"]
	if !ok || job.RunsOn != "windows-latest" {
		t.Fatal("scheduled Windows tests must run on windows-latest")
	}
	if job.Needs.Kind != 0 {
		t.Fatal("Windows tests must run independently of the full suite")
	}
	if job.If != "(github.event_name == 'schedule' || github.event_name == 'workflow_dispatch') && github.ref == 'refs/heads/develop'" {
		t.Fatal("Windows tests must be schedule/dispatch-gated on develop")
	}
	var commands []string
	var pinnedCheckout, goSetup bool
	for _, step := range job.Steps {
		commands = append(commands, step.Run)
		pinnedCheckout = pinnedCheckout || (step.Uses == "actions/checkout@v5" && step.With["ref"] == "${{ github.sha }}")
		goSetup = goSetup || (step.Uses == "actions/setup-go@v6" && step.With["go-version-file"] == "go.mod")
	}
	if !pinnedCheckout || !goSetup {
		t.Fatal("Windows tests must use the event SHA and repository Go version")
	}
	for _, pkg := range []string{"shell", "procgroup", "service", "update", "runnerauth"} {
		t.Run(pkg, func(t *testing.T) {
			for _, command := range commands {
				args := strings.Fields(command)
				if strings.HasPrefix(command, "go test ") && slices.Contains(args, "-short") && slices.Contains(args, "./internal/"+pkg) {
					return
				}
			}
			t.Errorf("Windows short tests missing internal/%s", pkg)
		})
	}
	installerTests := false
	for _, command := range commands {
		args := strings.Fields(command)
		installerTests = installerTests || (strings.HasPrefix(command, "go test ") && strings.Contains(command, "^TestPowerShellInstallScript") && slices.Contains(args, ".") && !slices.Contains(args, "-short"))
	}
	if !installerTests {
		t.Fatal("PowerShell installer tests must execute without -short")
	}
	report, ok := workflow.Jobs["report"]
	if !ok || report.Needs.Value != "windows-go" || report.If != "always() && !cancelled() && needs.windows-go.result == 'failure'" {
		t.Fatal("Windows failures must have an independent report job that excludes operator cancellation")
	}
	reporter := false
	for _, step := range report.Steps {
		if strings.Contains(step.Run, "go run ./tools/cifailure") && strings.Contains(step.Run, `select(.name == "Windows Go tests")`) && step.Env["CI_DEVELOP_SHA"] == "${{ github.sha }}" && step.Env["DETENT_API_KEY"] == "${{ secrets.DETENT_API_KEY }}" && step.Env["DETENT_MCP_URL"] != "" && step.Env["DETENT_PROJECT_ID"] != "" {
			reporter = true
		}
	}
	if !reporter {
		t.Fatal("Windows failures must use the existing native scheduled per-job reporter with pinned source identity")
	}
	for _, path := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		content := readNormalizedFile(t, path)
		for _, forbidden := range []string{"windows-go", "windows-tests.yml"} {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s must not depend on the scheduled Windows workflow", path)
			}
		}
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
		{name: "staging", dependency: "release", end: "\n  production:"},
		{name: "production", dependency: "[release, staging]"},
	} {
		t.Run(test.name+" release dependency", func(t *testing.T) {
			t.Parallel()
			job := workflowBetween(t, release, "\n  "+test.name+":\n", test.end)
			for _, want := range []string{
				"    needs: " + test.dependency + "\n",
				"    uses: ./.github/workflows/deploy-" + test.name + ".yml\n",
				"    with:\n      version: ${{ needs.release.outputs.version }}\n",
			} {
				if !strings.Contains(job, want) {
					t.Fatalf("release %s job missing %q", test.name, want)
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
