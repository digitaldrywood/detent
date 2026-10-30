package gotestreceipt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
)

const (
	helperEnvironment = "GOTESTRECEIPT_HELPER"
	helperRan         = "gotestreceipt helper ran"
	earlySource       = "package gotestreceipt\n\nimport \"os\"\n\nvar earlyMode = os.Getenv(\"GOTESTRECEIPT_EARLY\")\n"
)

var earlyMode = os.Getenv("GOTESTRECEIPT_EARLY")

func TestMain(m *testing.M) {
	if IsWrapperInvocation(os.Args[0]) {
		os.Exit(RunWrapper(os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

var testDigestKey = []byte(strings.Repeat("k", DigestKeyBytes))

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvironment) == "" {
		t.Skip("helper process only")
	}
	fmt.Println(helperRan)
	if os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "block" {
		select {}
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "DETENT_GO_TEST") || strings.Contains(entry, string(testDigestKey)) {
			t.Fatalf("test binary received receipt session state %s", name)
		}
	}
	path := os.Getenv("GOTESTRECEIPT_HELPER_PATH")
	switch os.Getenv("GOTESTRECEIPT_HELPER_MODE") {
	case "exec":
		output, err := exec.Command("/bin/sh", "script.sh").Output()
		if err != nil || strings.TrimSpace(string(output)) != "pass" {
			t.Fatalf("script output %q: %v", output, err)
		}
		return
	case "early":
		if earlyMode != "" {
			t.Fatalf("early mode %q", earlyMode)
		}
		return
	case "mtime":
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Year() < 2001 {
			t.Fatalf("stat %s: %v", path, err)
		}
		return
	case "remove":
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "pass" {
			t.Fatalf("input %q: %v", data, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	case "length":
		if limit, err := strconv.Atoi(os.Getenv("GOTESTRECEIPT_HELPER_LIMIT")); err != nil || len(path) > limit {
			t.Fatalf("path %q exceeds the limit: %v", path, err)
		}
		return
	}
	data, err := os.ReadFile("input.txt")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "rewrite" {
		if err := os.WriteFile("input.txt", data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if strings.TrimSpace(string(data)) != "pass" || os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "fail" {
		t.Fatalf("input %q", data)
	}
}

type memoryStore struct {
	mu       sync.Mutex
	receipts []Receipt
}

func (s *memoryStore) Attest(context.Context) (Attestation, error) {
	return Attestation{ID: "attestation", DigestKey: testDigestKey}, nil
}

func (s *memoryStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.receipts)
}

func (s *memoryStore) Lookup(_ context.Context, key string) ([]Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []Candidate
	for _, receipt := range s.receipts {
		if receipt.Key == key {
			candidates = append(candidates, Candidate{Inputs: receipt.Inputs, Digest: receipt.Digest})
		}
	}
	return candidates, nil
}

func (s *memoryStore) Record(_ context.Context, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, receipt)
	return nil
}

func TestRunWrapperReusesOnlyMatchingReceipts(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, "scratch with spaces")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, scratch)
	}
	pkg := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(pkg, "input.txt")
	t.Setenv(helperEnvironment, "1")
	t.Setenv("GOCOVERDIR", t.TempDir())
	helperSource := `package fixture
import ("testing"; "fmt"; "os"; "strings")
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GOTESTRECEIPT_HELPER") == "" {
		t.Skip("helper process only")
	}
	fmt.Println("gotestreceipt helper ran")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "DETENT_GO_TEST") || name == "GOTMPDIR" || strings.Contains(entry, "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk") {
			t.Fatalf("test binary received receipt session state %s", name)
		}
	}
	data, err := os.ReadFile("input.txt")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "rewrite" {
		if err := os.WriteFile("input.txt", data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if strings.TrimSpace(string(data)) != "pass" || os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "fail" {
		t.Fatalf("input %q", data)
	}
}

`
	for name, content := range map[string]string{"go.mod": "module example.org/fixture\n\ngo 1.26\n", "fixture_test.go": helperSource} {
		if err := os.WriteFile(filepath.Join(pkg, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := &memoryStore{}
	session, err := Start(t.Context(), store, nil, t.TempDir(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := session.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	executable := filepath.Join(session.workDir, "go-build1", "b001", "fixture.test")
	build := exec.CommandContext(t.Context(), "go", "test", "-c", "-o", executable)
	build.Dir = pkg
	procgroup.Configure(t.Context(), build)
	if err := procgroup.Start(build); err != nil {
		t.Fatal(err)
	}
	if err := procgroup.Wait(build); err != nil {
		t.Fatal(err)
	}
	t.Chdir(pkg)
	getenv := func(name string) string {
		switch name {
		case SocketEnvironment:
			return session.socket
		case RootEnvironment:
			return root
		}
		return os.Getenv(name)
	}
	args := []string{executable, "-test.paniconexit0", "-test.run=^TestHelperProcess$"}
	fakeBuild := stageTestBinary(t, filepath.Join(root, "go-build2"))
	outside := t.TempDir()
	for name, request := range map[string]executionRequest{
		"relative binary":                   {Args: append([]string{"fixture.test"}, args[1:]...), Directory: pkg, Root: root},
		"non-test program":                  {Args: []string{"/bin/sh", "-c", "exit 0"}, Directory: pkg, Root: root},
		"relative directory":                {Args: args, Directory: "pkg", Root: root},
		"binary under a fake go-build dir":  {Args: append([]string{fakeBuild}, args[1:]...), Directory: pkg, Root: root},
		"directory outside the module root": {Args: args, Directory: outside, Root: root},
		"binary directly in the work dir":   {Args: append([]string{filepath.Join(session.workDir, "x.test")}, args[1:]...), Directory: pkg, Root: root},
		"another checkout root":             {Args: args, Directory: pkg, Root: pkg},
		"filesystem root":                   {Args: args, Directory: pkg, Root: "/"},
	} {
		var stdout, stderr bytes.Buffer
		if _, err := requestExecution(session.socket, request, &stdout, &stderr); !errors.Is(err, errExecutionUnavailable) || strings.Contains(stdout.String(), helperRan) || store.count() != 0 {
			t.Fatalf("%s: err = %v, stdout = %q, receipts = %d; want refusal without execution or recording", name, err, stdout.String(), store.count())
		}
	}

	steps := []struct {
		name     string
		content  string
		mode     string
		wantCode int
		wantRun  bool
	}{
		{name: "first pass executes and records", content: "pass", wantRun: true},
		{name: "unchanged input reuses receipt", content: "pass"},
		{name: "changed input executes and fails", content: "changed", wantCode: 1, wantRun: true},
		{name: "restored input content with a new timestamp reuses the receipt", content: "pass"},
		{name: "changed environment executes", content: "pass", mode: "fail", wantCode: 1, wantRun: true},
		{name: "a test that rewrites its input executes and is not recorded", content: "pass", mode: "rewrite", wantRun: true},
	}
	for _, step := range steps {
		previous, err := os.ReadFile(input)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if string(previous) != step.content {
			if err := os.WriteFile(input, []byte(step.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("GOTESTRECEIPT_HELPER_MODE", step.mode)
		var stdout, stderr bytes.Buffer
		code := RunWrapper(args, getenv, nil, &stdout, &stderr)
		if code != step.wantCode {
			t.Fatalf("%s: code = %d, want %d\nstdout:\n%s\nstderr:\n%s", step.name, code, step.wantCode, stdout.String(), stderr.String())
		}
		if ran := strings.Contains(stdout.String(), helperRan); ran != step.wantRun {
			t.Fatalf("%s: helper ran = %t, want %t", step.name, ran, step.wantRun)
		}
	}
	stats := session.Stats()
	if stats.Hits != 2 || stats.Records != 1 || stats.Refused != 1 || stats.Failures != 0 {
		t.Fatalf("stats = %+v, want 2 hits, 1 record and 1 refusal", stats)
	}
	for name, value := range session.Environment() {
		if strings.Contains(value, string(testDigestKey)) || strings.Contains(value, base64.StdEncoding.EncodeToString(testDigestKey)) {
			t.Fatalf("validation command environment %s carries the digest key", name)
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		var conn net.Conn
		err := withSocketPath(session.socket, func(path string) error {
			var dialErr error
			conn, dialErr = dialer.DialContext(ctx, "unix", path)
			return dialErr
		})
		return conn, err
	}}
	defer transport.CloseIdleConnections()
	forged, err := (&http.Client{Transport: transport}).Post(clientBaseURL+"/record", "application/json", strings.NewReader(`{"key":"`+strings.Repeat("a", 64)+`","inputs":[],"digest":"`+strings.Repeat("b", 64)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := forged.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if forged.StatusCode != http.StatusNotFound || store.count() != 1 {
		t.Fatalf("forged receipt: status %d, receipts %d; want no record endpoint", forged.StatusCode, store.count())
	}
	if got := store.receipts[0]; got.Package != "pkg" || got.Attestation != "attestation" {
		t.Fatalf("receipt = %+v, want package pkg bound to the session attestation", got)
	}
	if !slices.Contains(store.receipts[0].Inputs, "open\tm\tpkg/input.txt") || !slices.Contains(store.receipts[0].Inputs, "env\tGOTESTRECEIPT_HELPER_MODE") {
		t.Fatalf("inputs = %q, want the helper's input file and mode variable", store.receipts[0].Inputs)
	}
}

func TestRunWrapperExecutesWhenUnobservedInputsChange(t *testing.T) {
	type fixture struct {
		root, pkg, scratch string
		env                map[string]string
	}
	tests := []struct {
		name    string
		mode    string
		prepare func(t *testing.T, f *fixture)
		change  func(t *testing.T, f *fixture)
	}{
		{
			name: "a script read by a subprocess changes",
			mode: "exec",
			prepare: func(t *testing.T, f *fixture) {
				writeFile(t, filepath.Join(f.pkg, "script.sh"), "cat data.txt\n")
				writeFile(t, filepath.Join(f.pkg, "data.txt"), "pass")
			},
			change: func(t *testing.T, f *fixture) { writeFile(t, filepath.Join(f.pkg, "data.txt"), "fail") },
		},
		{
			name:   "an environment variable read during package initialization changes",
			mode:   "early",
			change: func(t *testing.T, f *fixture) { f.env["GOTESTRECEIPT_EARLY"] = "fail" },
		},
		{
			name: "a temporary stat input changes only its modification time",
			mode: "mtime",
			prepare: func(t *testing.T, f *fixture) {
				f.env["GOTESTRECEIPT_HELPER_PATH"] = filepath.Join(f.scratch, "stamp")
				writeFile(t, f.env["GOTESTRECEIPT_HELPER_PATH"], "x")
				setTime(t, f.env["GOTESTRECEIPT_HELPER_PATH"], time.Now().Add(-time.Hour))
			},
			change: func(t *testing.T, f *fixture) {
				setTime(t, f.env["GOTESTRECEIPT_HELPER_PATH"], time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
			},
		},
		{
			name: "a checkout input the test deleted stays missing",
			mode: "remove",
			prepare: func(t *testing.T, f *fixture) {
				f.env["GOTESTRECEIPT_HELPER_PATH"] = filepath.Join(f.pkg, "fixture.txt")
				writeFile(t, f.env["GOTESTRECEIPT_HELPER_PATH"], "pass")
				setTime(t, f.pkg, time.Now().Add(-time.Hour))
			},
			change: func(*testing.T, *fixture) {},
		},
		{
			name: "a temporary input the test deleted stays missing",
			mode: "remove",
			prepare: func(t *testing.T, f *fixture) {
				f.env["GOTESTRECEIPT_HELPER_PATH"] = filepath.Join(f.scratch, "fixture.txt")
				writeFile(t, f.env["GOTESTRECEIPT_HELPER_PATH"], "pass")
				setTime(t, f.env["GOTESTRECEIPT_HELPER_PATH"], time.Now().Add(-time.Hour))
			},
			change: func(*testing.T, *fixture) {},
		},
		{
			name: "the checkout moves to a path of a different length",
			mode: "length",
			prepare: func(t *testing.T, f *fixture) {
				f.env["GOTESTRECEIPT_HELPER_PATH"] = f.root
				f.env["GOTESTRECEIPT_HELPER_LIMIT"] = strconv.Itoa(len(f.root))
			},
			change: func(t *testing.T, f *fixture) {
				f.root += "-moved"
				f.pkg = filepath.Join(f.root, "pkg")
				if err := os.MkdirAll(f.pkg, 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(f.pkg, "early_test.go"), earlySource)
				f.env["GOTESTRECEIPT_HELPER_PATH"] = f.root
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fixture{root: filepath.Join(t.TempDir(), "repo"), scratch: t.TempDir(), env: map[string]string{helperEnvironment: "1", "GOTESTRECEIPT_HELPER_MODE": tt.mode, "GOCOVERDIR": t.TempDir()}}
			f.pkg = filepath.Join(f.root, "pkg")
			if err := os.MkdirAll(f.pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(f.pkg, "early_test.go"), earlySource)
			if tt.prepare != nil {
				tt.prepare(t, f)
			}
			store := &memoryStore{}
			run := func() (int, string, Stats) {
				t.Helper()
				for name, value := range f.env {
					t.Setenv(name, value)
				}
				t.Chdir(f.pkg)
				session, err := Start(t.Context(), store, nil, t.TempDir(), f.root)
				if err != nil {
					t.Fatal(err)
				}
				executable := stageTestBinary(t, filepath.Join(session.workDir, "go-build1"))
				getenv := func(name string) string {
					switch name {
					case SocketEnvironment:
						return session.socket
					case RootEnvironment:
						return f.root
					}
					return os.Getenv(name)
				}
				var stdout, stderr bytes.Buffer
				code := RunWrapper([]string{executable, "-test.paniconexit0", "-test.run=^TestHelperProcess$"}, getenv, nil, &stdout, &stderr)
				stats, err := session.Close(context.WithoutCancel(t.Context()))
				if err != nil {
					t.Fatal(err)
				}
				return code, stdout.String() + stderr.String(), stats
			}
			if code, output, _ := run(); code != 0 || !strings.Contains(output, helperRan) {
				t.Fatalf("first run code = %d\n%s", code, output)
			}
			tt.change(t, f)
			code, output, stats := run()
			if !strings.Contains(output, helperRan) || code == 0 {
				t.Fatalf("second run code = %d, want the test to execute and fail after its input changed (stats %+v)\n%s", code, stats, output)
			}
		})
	}
}

func TestPreWindowInputs(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		want    []string
		wantErr bool
	}{
		{
			name:   "reads in tests run inside the logged window are not added",
			source: "package p\nimport (\"os\"; \"sync\"; \"testing\")\nvar lazy = sync.OnceValue(func() string { data, _ := os.ReadFile(\"x\"); return string(data) })\nfunc TestX(t *testing.T) { _ = os.Getenv(\"LATE\"); _, _ = os.ReadFile(\"late\") }\n",
		},
		{
			name:   "named reads in TestMain, init and package variables are added",
			source: "package p\nimport (sys \"os\"; \"testing\")\nconst mode = \"MODE\"\nvar early = sys.Getenv(\"EARLY\")\nfunc init() { _, _ = sys.LookupEnv(mode) }\nfunc TestMain(m *testing.M) { _ = sys.Getenv(\"MAIN\"); m.Run() }\n",
			want:   []string{"env\tEARLY", "env\tMAIN", "env\tMODE"},
		},
		{
			name:   "an unnamed read or a passed reader keys the whole environment",
			source: "package p\nimport (\"os\"; \"testing\")\nfunc run(func(string) string) {}\nfunc TestMain(m *testing.M) { run(os.Getenv); m.Run() }\n",
			want:   []string{environInput},
		},
		{
			name:   "listing the environment anywhere keys the whole environment because Go never logs it",
			source: "package p\nimport (\"os\"; \"testing\")\nfunc TestX(t *testing.T) { _ = os.Environ() }\n",
			want:   []string{environInput},
		},
		{
			name:    "a file read by a function TestMain calls is refused",
			source:  "package p\nimport (\"os\"; \"testing\")\nfunc setup() { _, _ = os.ReadFile(\"fixture\") }\nfunc TestMain(m *testing.M) { setup(); os.Exit(m.Run()) }\n",
			wantErr: true,
		},
		{
			name:    "a file read by a method TestMain calls is refused",
			source:  "package p\nimport (\"os\"; \"testing\")\ntype reader struct{}\nfunc (reader) Getenv(string) string { data, _ := os.ReadFile(\"input.txt\"); return string(data) }\nfunc TestMain(m *testing.M) { os := reader{}; _ = os.Getenv(\"INPUT\"); m.Run() }\n",
			wantErr: true,
		},
		{
			name:   "running the tests through the testing.M parameter follows no local Run method",
			source: "package p\nimport (\"os\"; \"testing\")\ntype server struct{}\nfunc (server) Run() int { _, _ = os.ReadFile(\"late\"); return 0 }\nfunc TestMain(m *testing.M) { os.Exit(m.Run()) }\n",
		},
		{
			name:    "a shadowed testing.M parameter follows local Run methods",
			source:  "package p\nimport (\"os\"; \"testing\")\ntype reader struct{}\nfunc (reader) Run() int { _, _ = os.ReadFile(\"input.txt\"); return 0 }\nfunc TestMain(m *testing.M) { { m := reader{}; m.Run() }; os.Exit(m.Run()) }\n",
			wantErr: true,
		},
		{
			name:    "a process started from TestMain is refused",
			source:  "package p\nimport (\"os\"; \"os/exec\"; \"testing\")\nfunc TestMain(m *testing.M) { _ = exec.Command(\"git\").Run(); os.Exit(m.Run()) }\n",
			wantErr: true,
		},
		{
			name:    "a glob in init is refused",
			source:  "package p\nimport \"path/filepath\"\nfunc init() { _, _ = filepath.Glob(\"*.yaml\") }\n",
			wantErr: true,
		},
		{
			name:    "a file reader taken as a value before the log is refused",
			source:  "package p\nimport (\"io/fs\"; \"os\"; \"testing\")\nvar read = fs.ReadFile\nfunc TestMain(m *testing.M) { load := os.ReadFile; _, _ = load(\"x\"); os.Exit(m.Run()) }\n",
			wantErr: true,
		},
		{
			name:    "files excluded by build constraints are still inspected",
			source:  "//go:build never\n\npackage p\nimport \"os\"\nfunc init() { _, _ = os.ReadFile(\"x\") }\n",
			wantErr: true,
		},
		{
			name:   "a local variable that shadows a constant keys the whole environment",
			source: "package p\nimport (\"os\"; \"testing\")\nconst name = \"FIXED\"\nfunc TestMain(m *testing.M) { name := os.Args[0]; _ = os.Getenv(name); os.Exit(m.Run()) }\n",
			want:   []string{environInput},
		},
		{
			name:    "an initializer that reads a file is refused",
			source:  "package p\nimport \"os\"\nvar input, err = os.ReadFile(\"fixture\")\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "p_test.go"), tt.source)
			got, err := PreWindowInputs(dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if len(got) != 0 || len(tt.want) != 0 {
				if !slices.Equal(got, tt.want) {
					t.Fatalf("inputs = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestSnapshotTempEntries(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "kept"), "x")
	entries, err := SnapshotTempEntries([]TempRoot{{"TMPDIR", root}, {"TMP", filepath.Join(root, "absent")}})
	if err != nil {
		t.Fatal(err)
	}
	if !entries[root] || !entries[filepath.Join(root, "kept")] || len(entries) != 2 {
		t.Fatalf("entries = %v, want the root and its entry", entries)
	}
	unreadable := filepath.Join(root, "unreadable")
	if err := os.Mkdir(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(unreadable, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := SnapshotTempEntries([]TempRoot{{"TMPDIR", unreadable}}); err == nil {
		t.Fatal("an unreadable temporary root was treated as empty")
	}
}

func TestEnvironmentListingKeepsOrder(t *testing.T) {
	digest := func(environ ...string) string {
		t.Helper()
		value, err := Digest([]string{environInput}, Environment{DigestKey: testDigestKey, Environ: func() []string { return environ }})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest("A=1", "A=2") == digest("A=2", "A=1") {
		t.Fatal("environment order that decides a duplicate's value did not change the digest")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setTime(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestParseTestLog(t *testing.T) {
	scratch := t.TempDir()
	kept, written := filepath.Join(scratch, "kept"), filepath.Join(scratch, "written")
	for _, path := range []string{kept, written} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now().Add(-time.Minute)
	if err := os.Chtimes(kept, start.Add(-time.Hour), start.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(scratch, "tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	for _, name := range []string{"pkg/testdata/a.txt", "docs/invariants.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, name), "x")
	}
	env := Environment{DigestKey: testDigestKey, Root: repo, GoRoot: "/opt/go", TempDirs: []TempRoot{{"TMPDIR", scratch}, {"TMP", repo + "/tmp"}}, UncacheableDirs: []string{"/dev", "/proc"}}
	snapshot := env
	snapshot.TempEntries = map[string]bool{kept: true, tool: true, filepath.Join(scratch, "shared"): true}
	tests := []struct {
		name    string
		log     string
		env     *Environment
		want    []string
		wantErr bool
	}{
		{
			name: "temporary files under entries created during the test are not inputs",
			log:  "# test log\nopen " + written + "\nopen " + filepath.Join(scratch, "TestX", "001", "db") + "\nopen " + kept + "\n",
			env:  &snapshot,
			want: []string{"open\tt:TMPDIR\tkept"},
		},
		{name: "a removed temporary file under an entry that predates the test is refused", log: "# test log\nopen " + filepath.Join(scratch, "shared", "fixture") + "\n", env: &snapshot, wantErr: true},
		{name: "a started program is refused", log: "# test log\nopen " + tool + "\n", env: &snapshot, wantErr: true},
		{name: "a missing relative open, which a subprocess may resolve elsewhere, is refused", log: "# test log\nopen tool.sh\n", wantErr: true},
		{name: "a missing relative stat is an input", log: "# test log\nstat optional.yaml\n", want: []string{"stat\tm\tpkg/optional.yaml"}},
		{
			name: "relative paths resolve from the package directory",
			log:  "# test log\nopen testdata/a.txt\nstat ../shared\ngetenv HOME\n",
			want: []string{"env\tHOME", "open\tm\tpkg/testdata/a.txt", "stat\tm\tshared"},
		},
		{
			name: "chdir records its directory and changes later relative paths",
			log:  "# test log\nchdir " + repo + "/docs\nopen invariants.md\n",
			want: []string{"open\tm\tdocs/invariants.md", "stat\tm\tdocs"},
		},
		{
			name:    "removed temporary inputs are refused",
			log:     "# test log\nopen " + scratch + "\nopen " + filepath.Join(scratch, "TestX", "001", "db") + "\nopen /dev/null\nopen /etc/hosts\n",
			wantErr: true,
		},
		{
			name:    "temporary files written during execution are refused",
			log:     "# test log\nopen " + kept + "\nopen " + written + "\nopen " + repo + "/tmp/config\n",
			wantErr: true,
		},
		{
			name: "temporary files that predate the test are named by their temporary root",
			log:  "# test log\nopen " + kept + "\n",
			want: []string{"open\tt:TMPDIR\tkept"},
		},
		{
			name: "toolchain reads are relative to GOROOT",
			log:  "# test log\nopen /opt/go/lib/time/zoneinfo.zip\nstat /opt/go\n",
			want: []string{"open\tg\tlib/time/zoneinfo.zip", "stat\tg\t."},
		},
		{name: "absolute host paths are refused", log: "# test log\nopen /etc/hosts\n", wantErr: true},
		{name: "impossible paths are not inputs", log: "# test log\nopen bad\x00name\n", want: []string{}},
		{name: "system reads are uncacheable", log: "# test log\nopen /proc/cpuinfo\n", wantErr: true},
		{name: "a stat of the device directory itself is refused as a host path", log: "# test log\nstat /dev\n", wantErr: true},
		{name: "device reads beyond the standard streams are uncacheable", log: "# test log\nopen /dev/shm/fixture\n", wantErr: true},
		{name: "missing header", log: "open a\n", wantErr: true},
		{name: "unknown operation", log: "# test log\nexec a\n", wantErr: true},
		{name: "empty log", log: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parseEnv := env
			if tt.env != nil {
				parseEnv = *tt.env
			}
			got, err := ParseTestLog(strings.NewReader(tt.log), filepath.Join(repo, "pkg"), parseEnv, start)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("inputs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDigestRelocatesOnlyEqualLengthCheckouts(t *testing.T) {
	base := t.TempDir()
	first, second, longer := filepath.Join(base, "one"), filepath.Join(base, "two"), filepath.Join(base, "three")
	for _, root := range []string{first, second, longer} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs := []string{"open\tm\tdata.txt", "stat\tm\tmissing", "env\tDETENT_WORKSPACE"}
	digest := func(root string) string {
		t.Helper()
		value, err := Digest(inputs, Environment{DigestKey: testDigestKey, Root: root, LookupEnv: func(string) (string, bool) { return root, true }})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest(first) != digest(second) {
		t.Fatal("digest differs between equal-length checkouts with identical inputs")
	}
	if digest(first) == digest(longer) {
		t.Fatal("digest hides a checkout path of a different length")
	}
	if err := os.WriteFile(filepath.Join(second, "data.txt"), []byte("diff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if digest(first) == digest(second) {
		t.Fatal("digest ignores a changed input file")
	}
	if _, err := Digest([]string{"open\tm\t../escape"}, Environment{DigestKey: testDigestKey, Root: first}); err == nil {
		t.Fatal("digest accepts a path outside the module root")
	}
}

func TestModifiedSince(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs := []string{"open\tm\tinput.txt", "stat\tm\t.", "env\tHOME"}
	env := Environment{Root: root}
	if err := ModifiedSince(inputs, env, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("unchanged inputs: %v", err)
	}
	if err := ModifiedSince(inputs, env, time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("an input written after the test started was accepted")
	}
}

func TestKeySeparatesArguments(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "pkg.test")
	if err := os.WriteFile(binary, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	concatenated := filepath.Join(t.TempDir(), "pkg.test")
	if err := os.WriteFile(concatenated, []byte("binary\narg 13 -test.run=^A$"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := make(map[string]string)
	for name, call := range map[string]struct {
		binary, workdir string
		args, environ   []string
	}{
		"joined arguments":                     {binary: binary, workdir: "m\tpkg", args: []string{"-test.run=^A$\narg 13 -test.skip=B"}},
		"split arguments":                      {binary: binary, workdir: "m\tpkg", args: []string{"-test.run=^A$", "-test.skip=B"}},
		"runtime environment":                  {binary: binary, workdir: "m\tpkg", args: []string{"-test.run=^A$", "-test.skip=B"}, environ: []string{"GOMAXPROCS=1"}},
		"argument":                             {binary: binary, workdir: "m\tpkg", args: []string{"-test.run=^A$"}},
		"binary bytes that spell the argument": {binary: concatenated, workdir: "m\tpkg"},
		"other package directory":              {binary: binary, workdir: "m\tother", args: []string{"-test.run=^A$"}},
	} {
		key, err := Key(call.binary, call.args, call.workdir, call.environ)
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := keys[key]; ok {
			t.Fatalf("%s and %s produced the same key", name, other)
		}
		keys[key] = name
	}
	unrelated, err := Key(binary, []string{"-test.run=^A$"}, "m\tpkg", []string{"DETENT_ISSUE_ID=example"})
	if err != nil {
		t.Fatal(err)
	}
	if keys[unrelated] != "argument" {
		t.Fatal("an environment variable outside the runtime set changed the key")
	}
}

func TestCacheable(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"-test.paniconexit0", "-test.timeout=10m0s", "-test.short=true", "-test.run=^TestX$"}, true},
		{[]string{"-test.v=true"}, false},
		{[]string{"-test.v"}, false},
		{[]string{"-test.v=false"}, true},
		{[]string{"-test.v=test2json"}, false},
		{[]string{"-test.coverprofile=c.out"}, false},
		{[]string{"-test.gocoverdir=/tmp/x"}, false},
		{[]string{"-test.count=3"}, false},
		{[]string{"-test.count=1"}, false},
		{[]string{"-test.bench=."}, false},
		{[]string{"-test.shuffle=on"}, false},
		{[]string{"-custom"}, false},
	}
	for _, tt := range tests {
		if got := Cacheable(tt.args); got != tt.want {
			t.Errorf("Cacheable(%q) = %t, want %t", tt.args, got, tt.want)
		}
	}
}

func TestGoflags(t *testing.T) {
	tests := []struct{ goflags, want string }{
		{"", "-trimpath -exec=/w"},
		{"-trimpath -p=2", "-trimpath -p=2 -exec=/w"},
		{"-p=2 -exec=/other", "-p=2 -exec=/other -trimpath"},
	}
	for _, tt := range tests {
		if got := Goflags(tt.goflags, "/w"); got != tt.want {
			t.Errorf("Goflags(%q) = %q, want %q", tt.goflags, got, tt.want)
		}
	}
}

func stageTestBinary(t *testing.T, buildDir string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(buildDir, "b001", "gotestreceipt.test")
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return staged
}

func TestRunTestStopsTheBinaryWhenTheExecutionEnds(t *testing.T) {
	t.Setenv(helperEnvironment, "1")
	t.Setenv("GOTESTRECEIPT_HELPER_MODE", "block")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	request := executionRequest{Directory: t.TempDir(), Environment: os.Environ()}
	if code := runTest(ctx, request, []string{executable, "-test.run=^TestHelperProcess$", "-test.timeout=2m"}, &stdout, &stderr); code == 0 || strings.Contains(stdout.String(), "test timed out") {
		t.Fatalf("code = 0 after the execution ended; stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunWrapperRunsTestsWhenTheSessionIsUnavailable(t *testing.T) {
	pkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(pkg, "input.txt"), []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperEnvironment, "1")
	t.Setenv("GOTESTRECEIPT_HELPER_MODE", "")
	t.Setenv("GOCOVERDIR", t.TempDir())
	t.Setenv(SocketEnvironment, filepath.Join(t.TempDir(), "missing.sock"))
	t.Setenv(RootEnvironment, pkg)
	t.Chdir(pkg)
	executable := stageTestBinary(t, filepath.Join(t.TempDir(), "go-build1"))
	var stdout, stderr bytes.Buffer
	code := RunWrapper([]string{executable, "-test.run=^TestHelperProcess$"}, os.Getenv, nil, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), helperRan) {
		t.Fatalf("code = %d, stdout = %q, stderr = %q; want the test to run without the session", code, stdout.String(), stderr.String())
	}
}

type attestingStore struct {
	attestation Attestation
	err         error
}

func (s attestingStore) Attest(context.Context) (Attestation, error) {
	return s.attestation, s.err
}

func (attestingStore) Lookup(context.Context, string) ([]Candidate, error) { return nil, nil }

func (attestingStore) Record(context.Context, Receipt) error { return nil }

func TestStartRefusesTheFilesystemRoot(t *testing.T) {
	for _, root := range []string{"/", "relative"} {
		if session, err := Start(t.Context(), &memoryStore{}, nil, t.TempDir(), root); err == nil {
			if _, closeErr := session.Close(t.Context()); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatalf("session started with module root %q", root)
		}
	}
}

func TestRunWrapperRunsTestsWithoutAValidAttestation(t *testing.T) {
	pkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(pkg, "input.txt"), []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(helperEnvironment, "1")
	t.Setenv("GOTESTRECEIPT_HELPER_MODE", "")
	t.Setenv("GOCOVERDIR", t.TempDir())
	t.Setenv(RootEnvironment, pkg)
	t.Chdir(pkg)
	for name, store := range map[string]attestingStore{
		"missing id":      {attestation: Attestation{DigestKey: testDigestKey}},
		"short key":       {attestation: Attestation{ID: "attestation", DigestKey: []byte("short")}},
		"control in id":   {attestation: Attestation{ID: "attest\nation", DigestKey: testDigestKey}},
		"oversized id":    {attestation: Attestation{ID: strings.Repeat("a", maxAttestation+1), DigestKey: testDigestKey}},
		"unavailable hub": {err: errors.New("hub unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			session, err := Start(t.Context(), store, nil, t.TempDir(), pkg)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(SocketEnvironment, session.socket)
			executable := stageTestBinary(t, filepath.Join(session.workDir, "go-build1"))
			var stdout, stderr bytes.Buffer
			code := RunWrapper([]string{executable, "-test.run=^TestHelperProcess$"}, os.Getenv, nil, &stdout, &stderr)
			stats, err := session.Close(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if code != 0 || !strings.Contains(stdout.String(), helperRan) || stats.Lookups != 0 || stats.Records != 0 || stats.Failures != 1 {
				t.Fatalf("code = %d, stats = %+v, stdout = %q; want the test to run with no receipt lookup or record", code, stats, stdout.String())
			}
		})
	}
}

func TestDigestHashesTheRecordedTemporaryRoot(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(root, "fixture"), []byte(root), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := Environment{DigestKey: testDigestKey, TempDirs: []TempRoot{{"TMPDIR", first}, {"TMP", second}}}
	inputs := []string{"stat\tt:TMP\tfixture"}
	before, err := Digest(inputs, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "fixture"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if unrelated, err := Digest(inputs, env); err != nil || unrelated != before {
		t.Fatalf("digest followed another temporary root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "fixture"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := Digest(inputs, env); err != nil || changed == before {
		t.Fatalf("digest ignored its own temporary root: %v", err)
	}
	if _, err := Digest([]string{"stat\tt:OTHER\tfixture"}, env); err == nil {
		t.Fatal("digest accepted an unknown temporary root")
	}
}

func TestValidateInputsRejectsHostPaths(t *testing.T) {
	for _, input := range []string{
		"open\ta\t/etc/hosts",
		"open\tm\t/etc/hosts",
		"stat\tm\t../outside",
		"open\tm\tC:/Users/example/input",
		"open\tm\tC:\\Users\\example\\input",
		"open\tm\t//server/share/input",
		"open\tg\t/etc/hosts",
		"open\tt\tkept",
		"open\tt:HOME\tkept",
	} {
		t.Run(input, func(t *testing.T) {
			if err := ValidateInputs([]string{input}); err == nil {
				t.Fatalf("accepted host path %q", input)
			}
		})
	}
}

func TestEnvironmentDigestsRequireSecretKey(t *testing.T) {
	inputs := []string{"env\tEXAMPLE_SECRET"}
	env := Environment{LookupEnv: func(string) (string, bool) { return "1234", true }}
	if _, err := Digest(inputs, env); err == nil {
		t.Fatal("accepted unkeyed secret digest")
	}
	env.DigestKey = []byte(strings.Repeat("a", 32))
	first, err := Digest(inputs, env)
	if err != nil {
		t.Fatal(err)
	}
	env.DigestKey = []byte(strings.Repeat("b", 32))
	second, err := Digest(inputs, env)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("digest does not depend on secret key")
	}
}
