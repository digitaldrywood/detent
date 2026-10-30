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
)

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
		name        string
		content     string
		mode        string
		wantCode    int
		wantRun     bool
		staleSource bool
	}{
		{name: "first pass executes and records", content: "pass", wantRun: true},
		{name: "unchanged input reuses receipt", content: "pass"},
		{name: "changed input executes and fails", content: "changed", wantCode: 1, wantRun: true},
		{name: "restored input with a new timestamp executes", content: "pass", wantRun: true},
		{name: "source changed after compilation cannot certify the old binary", content: "pass", wantRun: true, staleSource: true},
		{name: "changed environment executes", content: "pass", mode: "fail", wantCode: 1, wantRun: true},
		{name: "a test that rewrites its input executes and is not recorded", content: "pass", mode: "rewrite", wantRun: true},
	}
	for _, step := range steps {
		source := helperSource
		if step.staleSource {
			source = strings.ReplaceAll(source, "gotestreceipt helper ran", "different compiled behavior")
		}
		if err := os.WriteFile(filepath.Join(pkg, "fixture_test.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
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
	if stats.Hits != 1 || stats.Records != 2 || stats.Refused != 2 || stats.Failures != 0 {
		t.Fatalf("stats = %+v, want 1 hit, 2 records and 2 refusals", stats)
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
	if forged.StatusCode != http.StatusNotFound || store.count() != 2 {
		t.Fatalf("forged receipt: status %d, receipts %d; want no record endpoint", forged.StatusCode, store.count())
	}
	if got := store.receipts[0]; got.Package != "pkg" || got.Attestation != "attestation" {
		t.Fatalf("receipt = %+v, want package pkg bound to the session attestation", got)
	}
	if !slices.Contains(store.receipts[0].Inputs, "open\tm\tpkg/input.txt") || !slices.Contains(store.receipts[0].Inputs, "env\tGOTESTRECEIPT_HELPER_MODE") {
		t.Fatalf("inputs = %q, want the helper's input file and mode variable", store.receipts[0].Inputs)
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
	env := Environment{DigestKey: testDigestKey, Root: "/work/repo", GoRoot: "/opt/go", TempDirs: []TempRoot{{"TMPDIR", scratch}, {"TMP", "/work/repo/tmp"}}, UncacheableDirs: []string{"/dev", "/proc"}}
	tests := []struct {
		name    string
		log     string
		want    []string
		wantErr bool
	}{
		{
			name: "relative paths resolve from the package directory",
			log:  "# test log\nopen testdata/a.txt\nstat ../shared\ngetenv HOME\n",
			want: []string{"env\tHOME", "open\tm\tpkg/testdata/a.txt", "stat\tm\tshared"},
		},
		{
			name: "chdir records its directory and changes later relative paths",
			log:  "# test log\nchdir /work/repo/docs\nopen invariants.md\n",
			want: []string{"open\tm\tdocs/invariants.md", "stat\tm\tdocs"},
		},
		{
			name:    "removed temporary inputs are refused",
			log:     "# test log\nopen " + scratch + "\nopen " + filepath.Join(scratch, "TestX", "001", "db") + "\nopen /dev/null\nopen /etc/hosts\n",
			wantErr: true,
		},
		{
			name:    "temporary files written during execution are refused",
			log:     "# test log\nopen " + kept + "\nopen " + written + "\nopen /work/repo/tmp/config\n",
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
			got, err := ParseTestLog(strings.NewReader(tt.log), "/work/repo/pkg", env, start)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("inputs = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDigestPreservesCheckoutLocation(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs := []string{"open\tm\tdata.txt", "env\tDETENT_WORKSPACE"}
	digest := func(root, workspace string) string {
		t.Helper()
		value, err := Digest(inputs, Environment{DigestKey: testDigestKey, Root: root, LookupEnv: func(string) (string, bool) { return workspace, true }})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest(first, first) == digest(second, second) {
		t.Fatal("digest hides literal checkout paths")
	}
	if err := os.WriteFile(filepath.Join(second, "data.txt"), []byte("diff"), 0o600); err != nil {
		t.Fatal(err)
	}
	if digest(first, first) == digest(second, second) {
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
	joined, err := Key(binary, []string{"-test.run=^A$\narg 13 -test.skip=B"}, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	split, err := Key(binary, []string{"-test.run=^A$", "-test.skip=B"}, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := Key(binary, []string{"-test.run=^A$", "-test.skip=B"}, "", "", []string{"GOMAXPROCS=1"})
	if err != nil {
		t.Fatal(err)
	}
	if joined == split || limited == split {
		t.Fatal("distinct arguments or runtime environment produced the same key")
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
