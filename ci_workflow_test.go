package detent_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type requiredStatusCheck struct {
	name     string
	budget   string
	jobStart string
	jobEnd   string
	markers  []string
}

var requiredPRStatusChecks = []requiredStatusCheck{
	{
		name:     "Lint",
		budget:   "2m",
		jobStart: "  lint:",
		jobEnd:   "  verify:",
		markers:  []string{"name: Lint"},
	},
	{
		name:     "Verify (ubuntu-latest)",
		budget:   "8m",
		jobStart: "  verify:",
		jobEnd:   "  verify-fast:",
		markers:  []string{"name: Verify (ubuntu-latest)", "needs: [verify-fast, verify-race]", "FAST_RESULT", "RACE_RESULT"},
	},
	{
		name:     "Test Coverage",
		budget:   "4m",
		jobStart: "  test-cover:",
		jobEnd:   "  security:",
		markers:  []string{"name: Test Coverage", "make test-cover-packages"},
	},
	{
		name:     "Browser Visual",
		budget:   "15m",
		jobStart: "  browser-visual:",
		jobEnd:   "  portability-verify:",
		markers:  []string{"name: Browser Visual", "timeout-minutes: 15", "Run full browser visual gate", "Run browser smoke gate"},
	},
}

var integrationStatusChecks = []requiredStatusCheck{
	{
		name:     "Portability Verify (macos-latest)",
		budget:   "8m",
		jobStart: "  portability-verify:",
		jobEnd:   "  windows-core:",
		markers:  []string{"name: Portability Verify (${{ matrix.os }})", "os: [macos-latest, windows-latest]", "go build ./...", "go vet ./...", "go test ./..."},
	},
	{
		name:     "Portability Verify (windows-latest)",
		budget:   "45m",
		jobStart: "  portability-verify:",
		jobEnd:   "  windows-core:",
		markers:  []string{"name: Portability Verify (${{ matrix.os }})", "os: [macos-latest, windows-latest]", "go build ./...", "go vet ./...", "go test ./..."},
	},
	{
		name:     "Windows Core",
		budget:   "4m",
		jobStart: "  windows-core:",
		jobEnd:   "  installer-smoke:",
		markers:  []string{"name: Windows Core"},
	},
	{
		name:     "Installer Smoke (ubuntu-latest)",
		budget:   "6m",
		jobStart: "  installer-smoke:",
		jobEnd:   "  goreleaser-snapshot:",
		markers:  []string{"name: Installer Smoke (${{ matrix.os }})", "os: [ubuntu-latest, windows-latest]"},
	},
	{
		name:     "Installer Smoke (windows-latest)",
		budget:   "6m",
		jobStart: "  installer-smoke:",
		jobEnd:   "  goreleaser-snapshot:",
		markers:  []string{"name: Installer Smoke (${{ matrix.os }})", "os: [ubuntu-latest, windows-latest]"},
	},
	{
		name:     "GoReleaser Snapshot",
		budget:   "15m",
		jobStart: "  goreleaser-snapshot:",
		jobEnd:   "  report-integration-failures:",
		markers:  []string{"name: GoReleaser Snapshot", "timeout-minutes: 15", "args: release --snapshot --clean", "MINISIGN_KEY_FILE: ${{ runner.temp }}/detent-minisign.key"},
	},
}

func TestCIConcurrencyKeepsMainPushRuns(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	concurrency := workflowBetween(t, workflow, "concurrency:\n", "\njobs:")
	for _, want := range []string{
		"group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || github.run_id }}",
		"cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
	} {
		if !strings.Contains(concurrency, want) {
			t.Fatalf("CI concurrency missing %q", want)
		}
	}
}

func TestSnapshotBudgetPreservesReleaseWork(t *testing.T) {
	t.Parallel()

	config := readNormalizedFile(t, ".goreleaser.yaml")
	for _, tt := range []struct {
		name    string
		start   string
		end     string
		markers []string
	}{
		{"hooks", "before:", "\nbuilds:", []string{"go mod download", "go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0", "make generate", "go test ./..."}},
		{"targets", "builds:", "\narchives:", []string{"CGO_ENABLED=0", "-trimpath", "- darwin", "- linux", "- windows", "- amd64", "- arm64"}},
		{"archives", "archives:", "\nbrews:", []string{"- tar.gz", "goos: windows", "- zip", "- README.md", "- LICENSE", "- docs/**/*", "- scripts/hub-smoke.py"}},
		{"packages", "nfpms:", "\nscoops:", []string{"- deb", "- rpm"}},
		{"checksums", "checksum:", "\nsigns:", []string{"algorithm: sha256"}},
		{"signing", "signs:", "\nsnapshot:", []string{"artifacts: checksum", "cmd: minisign", "${artifact}.minisig", "{{ .Env.MINISIGN_KEY_FILE }}"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			section := workflowBetween(t, config, tt.start, tt.end)
			for _, marker := range tt.markers {
				if !strings.Contains(section, marker) {
					t.Errorf("release configuration missing %q", marker)
				}
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
		{name: "test", want: "$(GO_TEST) ./..."},
		{name: "test-race", want: "$(GO_TEST) -race $$packages"},
		{name: "test-race-hub", want: "env -u DETENT_API_TOKEN go run ./tools/testgate -race"},
		{name: "test-cover", want: "$(GO_TEST) -coverprofile=$(COVERPROFILE_RAW) ./..."},
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
		`GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" "$(GOLANGCI_LINT)" run --timeout=5m`,
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

	workflow := workflowBetween(t, readNormalizedFile(t, ".github/workflows/ci.yml"), "  lint:", "\n  verify:")
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
			if !strings.Contains(string(output), "pinned:go1.26.6:run --timeout=5m") {
				t.Fatalf("make lint did not invoke the pinned toolchain: %s", output)
			}
			if installed := strings.Contains(string(output), "go install"); installed == cached {
				t.Fatalf("make lint installed = %v, cached = %v: %s", installed, cached, output)
			}
		})
	}
}

func TestMainProtectionDocumentationMatchesWorkflow(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	docs := readNormalizedFile(t, "docs/execution-seams.md")
	protection := workflowBetween(t, docs, "### Main Branch Protection\n", "\n## Still Git/PR Coupled")

	for _, want := range []string{
		"`required_status_checks.strict: true`",
		"must not report success from a path- or event-dependent no-op",
		"`gate.required_status_checks`",
		"`cancel-in-progress: ${{ github.event_name == 'pull_request' }}`",
		"`Browser Visual`",
	} {
		if !strings.Contains(protection, want) {
			t.Fatalf("main branch protection docs missing %q", want)
		}
	}

	for _, check := range requiredPRStatusChecks {
		if !strings.Contains(protection, "- `"+check.name+"` - budget: `"+check.budget+"`") {
			t.Fatalf("main branch protection docs missing required check %q", check.name)
		}

		job := workflowBetween(t, workflow, check.jobStart, check.jobEnd)
		for _, marker := range check.markers {
			if !strings.Contains(job, marker) {
				t.Fatalf("workflow job for required check %q missing %q", check.name, marker)
			}
		}
	}
}

func TestRequiredChecksDoNotUseEventDependentGreenNoops(t *testing.T) {
	t.Parallel()

	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	for _, check := range requiredPRStatusChecks {
		job := workflowBetween(t, workflow, check.jobStart, check.jobEnd)
		for _, forbidden := range []string{
			"EVENT_NAME",
			"steps.policy.outputs",
			"Skip ",
			" skipped:",
		} {
			if strings.Contains(job, forbidden) {
				t.Fatalf("required check %q contains green no-op marker %q", check.name, forbidden)
			}
		}
	}
}

func TestIntegrationChecksRunOnlyOnMainPushOrDispatch(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	for _, check := range integrationStatusChecks {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			job := workflowBetween(t, workflow, check.jobStart, check.jobEnd)
			want := "    if: github.event_name == 'workflow_dispatch' || (github.event_name == 'push' && github.ref == 'refs/heads/main')"
			if !strings.Contains(job, want) {
				t.Fatalf("integration job %q must run only on main pushes or dispatch", check.name)
			}
			for _, marker := range check.markers {
				if !strings.Contains(job, marker) {
					t.Fatalf("integration job %q missing %q", check.name, marker)
				}
			}
		})
	}
	security := workflowBetween(t, workflow, "  security:", "  browser-visual:")
	if !strings.Contains(security, "github.event.pull_request.draft == false") || !strings.Contains(security, "make security") {
		t.Fatal("Security must continue running on every ready PR")
	}
	reporter := workflowBetween(t, workflow, "  report-integration-failures:", "")
	for _, marker := range []string{
		"needs: [portability-verify, windows-core, installer-smoke, goreleaser-snapshot]",
		"if: failure() && github.ref == 'refs/heads/main' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch')",
		"/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100",
		"go run ./tools/cifailure",
	} {
		if !strings.Contains(reporter, marker) {
			t.Fatalf("failure reporting missing %q", marker)
		}
	}
}

func TestPortabilityStressRunsOutsidePullRequestGate(t *testing.T) {
	t.Parallel()

	requiredWorkflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	requiredJob := workflowBetween(t, requiredWorkflow, "  portability-verify:", "\n  windows-core:")
	for _, forbidden := range []string{"go test -race", "-count=10", "-count=20"} {
		if strings.Contains(requiredJob, forbidden) {
			t.Fatalf("required portability job contains heavy coverage %q", forbidden)
		}
	}

	stressWorkflow := readNormalizedFile(t, ".github/workflows/portability-stress.yml")
	for _, want := range []string{
		"schedule:",
		"workflow_dispatch:",
		"timeout-minutes: 45",
		"os: [macos-latest, windows-latest]",
		"go test ./internal/orchestrator -run '^TestLocalSQLiteArtifactLifecycleEndToEnd$' -count=20",
		"go test -race ./internal/cli ./internal/runner ./tools/checklock -count=10 -timeout=30m",
		"go test -race ./...",
	} {
		if !strings.Contains(stressWorkflow, want) {
			t.Fatalf("portability stress workflow missing %q", want)
		}
	}
	if strings.Contains(stressWorkflow, "pull_request:") {
		t.Fatal("portability stress workflow must not run for every pull request")
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
		"falling back to go install",
		"Release installer fell back to go install",
		"exit 1",
		"Verified checksum for detent_",
	} {
		if !strings.Contains(linux, want) {
			t.Fatalf("Linux installer smoke step missing %q", want)
		}
	}

	windows := workflowBetween(t, job, "      - name: Smoke release installer\n        if: runner.os == 'Windows'", "")
	for _, want := range []string{
		"falling back to go install",
		"Release installer fell back to go install",
		"Verified checksum for detent_.*_windows_.*\\.zip",
	} {
		if !strings.Contains(windows, want) {
			t.Fatalf("Windows installer smoke step missing %q", want)
		}
	}
}

func TestBrowserVisualGateCoversBoardInteractions(t *testing.T) {
	t.Parallel()

	workflowRaw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ReadFile(.github/workflows/ci.yml) error = %v", err)
	}
	workflow := strings.ReplaceAll(string(workflowRaw), "\r\n", "\n")
	visualJob := workflowBetween(t, workflow, "  browser-visual:", "\n  portability-verify:")
	for _, want := range []string{
		"npm run test:visual",
		"tmp/detent --help",
		"go.mod|go.sum",
		"name: Upload browser visual evidence",
		"tmp/playwright-evidence",
		"name: Upload browser visual failure artifacts",
		"tmp/playwright-report",
		"tmp/playwright-results",
	} {
		if !strings.Contains(visualJob, want) {
			t.Fatalf("browser visual job missing %q", want)
		}
	}

	visualSpecRaw, err := os.ReadFile("tests/visual/layout.spec.js")
	if err != nil {
		t.Fatalf("ReadFile(tests/visual/layout.spec.js) error = %v", err)
	}
	visualSpec := strings.ReplaceAll(string(visualSpecRaw), "\r\n", "\n")
	for _, want := range []string{
		`test("board card opens the detail sheet"`,
		`[data-detail-sheet]`,
		`test("board lane picker hides and restores lanes"`,
		`test("board applies snapshot updates without reload"`,
	} {
		if !strings.Contains(visualSpec, want) {
			t.Fatalf("browser visual spec missing %q", want)
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

func TestCIDraftAndVerifyDependencies(t *testing.T) {
	t.Parallel()
	workflow := readNormalizedFile(t, ".github/workflows/ci.yml")
	if !strings.Contains(workflow, "types: [opened, synchronize, reopened, ready_for_review]") {
		t.Fatal("PR CI must run on readiness and later head updates")
	}
	for _, job := range []string{"lint", "verify", "verify-fast", "verify-race", "test-cover", "security", "browser-visual"} {
		t.Run(job, func(t *testing.T) {
			section := workflowBetween(t, workflow, "  "+job+":\n", "    steps:")
			if !strings.Contains(section, "github.event_name != 'pull_request' || github.event.pull_request.draft == false") {
				t.Fatal("job must skip drafts and retain non-PR triggers")
			}
		})
	}
	aggregate := workflowBetween(t, workflow, "  verify:\n", "  verify-fast:\n")
	for _, want := range []string{"needs: [verify-fast, verify-race]", "if: always()", `test "$FAST_RESULT" = success && test "$RACE_RESULT" = success`} {
		if !strings.Contains(aggregate, want) {
			t.Errorf("aggregate must reject failed, cancelled and skipped dependencies: missing %q", want)
		}
	}
	race := workflowBetween(t, workflow, "  verify-race:\n", "  test-cover:\n")
	for _, want := range []string{"shard: [0, 1, 2, 3]", "fail-fast: false", "~/go/pkg/mod", "~/.cache/go-build", "hashFiles('go.sum')", `bash scripts/ci-race-shard.sh "$SHARD"`} {
		if !strings.Contains(race, want) {
			t.Errorf("race shards missing %q", want)
		}
	}
}

func TestCIRacePartition(t *testing.T) {
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
	}{
		{"success", "2", "0", "0", true, true},
		{"discovery failure", "2", "1", "0", false, false},
		{"race failure survives tee", "2", "0", "1", false, true},
		{"invalid shard", "4", "0", "0", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "bin"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range []string{"ci-race-shard.sh", "ci-race-packages.awk"} {
				if err := os.WriteFile(filepath.Join(root, "scripts", file), []byte(readNormalizedFile(t, "scripts/"+file)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fakeGo := `#!/bin/sh
case "$1" in
list)
  printf '%s\n' github.com/digitaldrywood/detent/internal/cli github.com/digitaldrywood/detent/internal/config
  exit "$LIST_EXIT"
  ;;
test)
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
			if tc.wantTests {
				data, err := os.ReadFile(filepath.Join(root, "tmp", "shard-2-race-evidence", "tests.jsonl"))
				if err != nil || !strings.Contains(string(data), `"Action":"pass"`) {
					t.Fatalf("missing race evidence: %v: %s", err, data)
				}
			}
		})
	}
}
