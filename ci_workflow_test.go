package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestCIHasNoPullRequestConcurrency(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	if strings.Contains(workflow, "github.event.pull_request") || strings.Contains(workflow, "pull_request:") {
		t.Fatal("scheduled CI must not start for pull requests")
	}
	if !strings.Contains(workflow, "group: scheduled-develop-validation") {
		t.Fatal("scheduled validation must avoid overlapping tag publication")
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

	tests := []struct {
		name string
		want string
	}{
		{name: "test", want: "$(GO_TEST) $$packages"},
		{name: "test-web", want: "$(GO_TEST) ./internal/web"},
		{name: "test-race", want: "$(GO_TEST) -race $$packages"},
		{name: "test-race-hub-a", want: "env -u DETENT_API_TOKEN go run ./tools/testgate -race -parallel $(HUB_RACE_PARALLEL) -timeout $(HUB_RACE_TIMEOUT) -run"},
		{name: "test-race-hub-b", want: "env -u DETENT_API_TOKEN go run ./tools/testgate -race -parallel $(HUB_RACE_PARALLEL) -timeout $(HUB_RACE_TIMEOUT) -skip"},
		{name: "test-cover", want: "$(GO_TEST) -coverprofile=tmp/rest-cover.raw.out $$packages"},
		{name: "test-cover-web", want: "$(GO_TEST) -coverprofile=tmp/web-cover.raw.out ./internal/web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(makefile, tt.want) {
				t.Fatalf("Makefile missing %q", tt.want)
			}
		})
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
		`GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" GOBIN="$(GOLANGCI_LINT_DIR)" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)`,
		"setup: $(GOLANGCI_LINT)",
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
		"run: make lint",
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
			write(filepath.Join(root, ".golangci-version"), "v2.9.0\n")
			write(filepath.Join(root, "go.mod"), "module example.com/test\n\ngo 1.26\n\ntoolchain go1.26.6\n")
			bin := filepath.Join(root, "bin")
			write(filepath.Join(bin, "golangci-lint"), "#!/bin/sh\necho incompatible ambient linter >&2\nexit 99\n")
			linter := "#!/bin/sh\nprintf 'pinned:%s:%s\\n' \"$GOTOOLCHAIN\" \"$*\"\n"
			fixture := filepath.Join(root, "pinned-linter")
			write(fixture, linter)
			write(filepath.Join(bin, "go"), "#!/bin/sh\nset -eu\n[ \"$GOTOOLCHAIN\" = go1.26.6 ]\n[ \"$*\" = 'install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.9.0' ]\ncp pinned-linter \"$GOBIN/golangci-lint\"\n")
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
			if installed := strings.Contains(string(output), "go install"); installed == cached {
				t.Fatalf("make lint installed = %v, cached = %v: %s", installed, cached, output)
			}
		})
	}
}

func TestCIRunsOnScheduleAndManualDispatch(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	triggers := workflowBetween(t, workflow, "on:\n", "\npermissions:")
	for _, want := range []string{"schedule:", "cron: '17 * * * *'", "workflow_dispatch:", "fail_job:"} {
		if !strings.Contains(triggers, want) {
			t.Errorf("CI triggers missing %q", want)
		}
	}
	for _, forbidden := range []string{"pull_request:", "merge_group:", "push:"} {
		if strings.Contains(triggers, forbidden) {
			t.Errorf("CI has forbidden trigger %q", forbidden)
		}
	}
}

func TestScheduledCIValidatesPinnedDevelopmentSHA(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	if !strings.Contains(workflow, "git ls-remote origin refs/heads/develop") || !strings.Contains(workflow, "echo \"develop_sha=$develop_sha\" >> \"$GITHUB_OUTPUT\"") {
		t.Fatal("scheduled CI must resolve and pin develop")
	}
	if count := strings.Count(workflow, "ref: ${{ needs.preflight.outputs.develop_sha }}"); count < 12 {
		t.Fatalf("only %d jobs checkout the pinned SHA", count)
	}
	for _, want := range []string{"make test", "make security", "make test-cover-packages", "npm run test:visual", "make check-invariants"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("scheduled full suite missing %q", want)
		}
	}
}

func TestInstallerSmokeUsesAuthenticatedReleaseVersion(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	job := workflowBetween(t, workflow, "  installer-smoke:", "\n  goreleaser-snapshot:")

	for _, want := range []string{
		"name: Resolve release installer version",
		"GH_TOKEN: ${{ github.token }}",
		"bash ./scripts/resolve-release-tag.sh",
		"DETENT_VERSION=$tag",
		"$GITHUB_ENV",
	} {
		if !strings.Contains(job, want) {
			t.Fatalf("installer-smoke job missing %q", want)
		}
	}

	linux := workflowBetween(t, job, "      - name: Smoke release installer\n        if: runner.os == 'Linux'", "      - name: Smoke release installer\n        if: runner.os == 'Windows'")
	for _, want := range []string{
		"2>&1",
		"falling back to prepared source",
		"Release installer fell back to prepared source",
		"exit 1",
		"Verified checksum for detent_",
	} {
		if !strings.Contains(linux, want) {
			t.Fatalf("Linux installer smoke step missing %q", want)
		}
	}

	windows := workflowBetween(t, job, "      - name: Smoke release installer\n        if: runner.os == 'Windows'", "")
	for _, want := range []string{
		"falling back to prepared source",
		"Release installer fell back to prepared source",
		"Verified checksum for detent_.*_windows_.*\\.zip",
	} {
		if !strings.Contains(windows, want) {
			t.Fatalf("Windows installer smoke step missing %q", want)
		}
	}
}

func TestBrowserVisualGateCoversBoardInteractions(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	visual := workflowBetween(t, workflow, "  browser-visual-shard:", "\n  portability-verify:")
	for _, want := range []string{"npm run test:visual", "--shard=${{ matrix.shard }}/3", "name: Upload browser visual evidence", "name: Upload browser visual failure artifacts"} {
		if !strings.Contains(visual, want) {
			t.Errorf("browser visual job missing %q", want)
		}
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

func TestScheduledCIJobDependencies(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	for _, name := range []string{"invariants", "lint", "verify-fast", "verify-race", "test-cover", "security", "browser-visual-shard", "portability-verify", "windows-core", "installer-smoke", "goreleaser-snapshot"} {
		if !strings.Contains(workflow, "  "+name+":\n    needs: preflight\n    if: needs.preflight.outputs.should_run == 'true'") {
			t.Errorf("%s must depend on preflight", name)
		}
	}
	if !strings.Contains(workflow, "shard: [0, 1, 2, 3, 4, 5]") || !strings.Contains(workflow, "shard: [1, 2, 3]") {
		t.Fatal("full suite lost race or visual shards")
	}
}

func TestCIRacePartition(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()
	awk, err := exec.LookPath("awk")
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Linux CI partition uses awk")
		}
		t.Fatal(err)
	}
	const prefix = "github.com/digitaldrywood/detent/"
	packages := []string{prefix + "internal/hubserver", prefix + "internal/orchestrator", prefix + "internal/cli", prefix + "internal/config", prefix + "tools/testgate", "github.com/digitaldrywood/detent"}
	partition := func(input []string) map[string]int {
		t.Helper()
		result := make(map[string]int)
		for _, shard := range []string{"0", "1", "2", "3"} {
			cmd := exec.CommandContext(t.Context(), awk, "-v", "shard="+shard, "-f", "scripts/ci-race-packages.awk")
			cmd.Stdin = strings.NewReader(strings.Join(input, "\n") + "\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("partition: %v: %s", err, out)
			}
			for _, pkg := range strings.Fields(string(out)) {
				if _, exists := result[pkg]; exists {
					t.Fatalf("package %s assigned more than once", pkg)
				}
				result[pkg] = int(shard[0] - '0')
			}
		}
		if len(result) != len(input) {
			t.Fatalf("assigned %d of %d packages", len(result), len(input))
		}
		return result
	}
	original := partition(packages)
	for _, tc := range []struct {
		name  string
		input []string
	}{
		{"reordered", []string{packages[5], packages[4], packages[3], packages[2], packages[1], packages[0]}},
		{"added", append(append([]string{}, packages...), prefix+"internal/newpackage")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := partition(tc.input)
			for pkg, shard := range original {
				if got[pkg] != shard {
					t.Errorf("%s moved from shard %d to %d", pkg, shard, got[pkg])
				}
			}
		})
	}
	for _, tc := range []struct {
		pkg   string
		shard int
	}{{packages[0], 0}, {packages[1], 1}} {
		if original[tc.pkg] != tc.shard {
			t.Errorf("%s must have dedicated shard %d", tc.pkg, tc.shard)
		}
	}
	for _, pkg := range packages[2:] {
		if original[pkg] < 2 {
			t.Errorf("%s shares a dedicated runner", pkg)
		}
	}
}

func TestCIRaceShardFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	if runtime.GOOS == "windows" {
		t.Skip("Linux CI runner script uses bash")
	}
	for _, tc := range []struct {
		name        string
		shard       string
		listExit    string
		testExit    string
		wantSuccess bool
		wantTests   bool
		wantMake    string
	}{
		{"success", "2", "0", "0", true, true, ""},
		{"workspace shard", "3", "0", "0", true, true, ""},
		{"workspace failure", "3", "0", "1", false, false, ""},
		{"discovery failure", "2", "1", "0", false, false, ""},
		{"race failure survives tee", "2", "0", "1", false, true, ""},
		{"hub partition a", "0", "0", "0", true, false, "test-race-hub-a"},
		{"hub partition a failure", "0", "0", "1", false, false, "test-race-hub-a"},
		{"orchestrator", "1", "0", "0", true, false, "test-race-orchestrator"},
		{"hub partition b", "4", "0", "0", true, false, "test-race-hub-b"},
		{"hub partition c", "5", "0", "0", true, false, "test-race-hub-c"},
		{"invalid shard", "6", "0", "0", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "bin"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range []string{"ci-race-shard.sh", "ci-race-packages.awk", "test-workspace.sh"} {
				if err := os.WriteFile(filepath.Join(root, "scripts", file), []byte(readNormalizedFile(t, "scripts/"+file)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fakeGo := `#!/bin/sh
case "$1" in
list)
  printf '%s\n' github.com/digitaldrywood/detent/internal/cli github.com/digitaldrywood/detent/internal/config github.com/digitaldrywood/detent/internal/workspace
  exit "$LIST_EXIT"
  ;;
run)
  test -z "${DETENT_API_TOKEN:-}" || exit 99
  case "$*" in *"-timeout "*) ;; *) exit 97 ;; esac
  echo invoked > workspace-invoked
  exit "$TEST_EXIT"
  ;;
test)
  case "$*" in *internal/workspace*) exit 96 ;; esac
  test -z "${DETENT_API_TOKEN:-}" || exit 99
  echo invoked > invoked
  echo '{"Action":"pass"}'
  exit "$TEST_EXIT"
  ;;
esac
exit 98
`
			if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte(fakeGo), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "bin", "make"), []byte("#!/bin/sh\necho \"$*\" > make-invoked\nexit \"$TEST_EXIT\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "scripts/ci-race-shard.sh", tc.shard)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"), "LIST_EXIT="+tc.listExit, "TEST_EXIT="+tc.testExit, "DETENT_API_TOKEN=fixture-token")
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("success=%v, want %v: %s", err == nil, tc.wantSuccess, out)
			}
			_, err = os.Stat(filepath.Join(root, "invoked"))
			if (err == nil) != tc.wantTests {
				t.Fatalf("tests invoked=%v, want %v: %s", err == nil, tc.wantTests, out)
			}
			made, err := os.ReadFile(filepath.Join(root, "make-invoked"))
			if strings.TrimSpace(string(made)) != tc.wantMake || (err == nil) != (tc.wantMake != "") {
				t.Fatalf("make target = %q (%v), want %q: %s", made, err, tc.wantMake, out)
			}
			if tc.wantTests {
				data, err := os.ReadFile(filepath.Join(root, "tmp", "shard-"+tc.shard+"-race-evidence", "tests.jsonl"))
				if err != nil || !strings.Contains(string(data), `"Action":"pass"`) {
					t.Fatalf("missing race evidence: %v: %s", err, data)
				}
			}
		})
	}
}

func TestHubRacePartitionsCoverEveryTestOnce(t *testing.T) {
	t.Parallel()
	makefile := readNormalizedFile(t, "Makefile")
	values := map[string]string{}
	for line := range strings.SplitSeq(makefile, "\n") {
		for _, name := range []string{"HUB_RACE_PARTITION", "HUB_RACE_PARTITION_B"} {
			if value, ok := strings.CutPrefix(line, name+" := "); ok {
				values[name] = strings.TrimSpace(value)
			}
		}
	}
	for _, name := range []string{"HUB_RACE_PARTITION", "HUB_RACE_PARTITION_B"} {
		if values[name] == "" || strings.Contains(values[name], "/") {
			t.Fatalf("%s = %q, want a nonempty top-level test pattern", name, values[name])
		}
	}
	for _, want := range []string{
		"test-race-hub: test-race-hub-a test-race-hub-b test-race-hub-c\n",
		"-run '$(HUB_RACE_PARTITION)' -output tmp/hub-race-evidence-a ./internal/hubserver\n",
		"-run '$(HUB_RACE_PARTITION_B)' -output tmp/hub-race-evidence-b ./internal/hubserver\n",
		"-skip '$(HUB_RACE_PARTITION)|$(HUB_RACE_PARTITION_B)' -output tmp/hub-race-evidence-c ./internal/hubserver\n",
	} {
		if !strings.Contains(makefile, want) {
			t.Errorf("Hub race partitions must run A, run B and skip both for C: missing %q", want)
		}
	}
	shard := readNormalizedFile(t, "scripts/ci-race-shard.sh")
	for _, want := range []string{"0) exec make test-race-hub-a ;;", "4) exec make test-race-hub-b ;;", "5) exec make test-race-hub-c ;;"} {
		if !strings.Contains(shard, want) {
			t.Errorf("CI race shards missing %q", want)
		}
	}
	if testing.Short() {
		t.Skip("listing Hub tests builds the package")
	}
	a, err := regexp.Compile(values["HUB_RACE_PARTITION"])
	if err != nil {
		t.Fatalf("compile HUB_RACE_PARTITION: %v", err)
	}
	b, err := regexp.Compile(values["HUB_RACE_PARTITION_B"])
	if err != nil {
		t.Fatalf("compile HUB_RACE_PARTITION_B: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), "go", "test", "-list", ".", "./internal/hubserver").Output()
	if err != nil {
		t.Fatalf("list Hub tests: %v", err)
	}
	counts := [3]int{}
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasPrefix(name, "Test") && !strings.HasPrefix(name, "Example") && !strings.HasPrefix(name, "Fuzz") {
			continue
		}
		inA, inB := a.MatchString(name), b.MatchString(name)
		switch {
		case inA && inB:
			t.Fatalf("%s runs in both partition A and partition B", name)
		case inA:
			counts[0]++
		case inB:
			counts[1]++
		default:
			counts[2]++
		}
	}
	if counts[0] == 0 || counts[1] == 0 || counts[2] == 0 {
		t.Fatalf("partition sizes = %v, want all three Hub race partitions nonempty", counts)
	}
}
