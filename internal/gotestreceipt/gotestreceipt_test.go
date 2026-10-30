package gotestreceipt

import (
	"bytes"
	"context"
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

type memoryStore struct {
	mu       sync.Mutex
	receipts []Receipt
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
	for _, name := range []string{"DETENT_GO_TEST_RECEIPTS", "DETENT_GO_TEST_RECEIPTS_TOKEN", "DETENT_GO_TEST_ROOT"} {
		if _, ok := os.LookupEnv(name); ok {
			t.Fatalf("test binary received %s", name)
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
	executable := filepath.Join(root, "fixture.test")
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

	store := &memoryStore{}
	session, err := Start(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := session.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	getenv := func(name string) string {
		switch name {
		case SocketEnvironment:
			return session.socket
		case TokenEnvironment:
			return session.token
		case RootEnvironment:
			return root
		}
		return os.Getenv(name)
	}
	args := []string{executable, "-test.paniconexit0", "-test.run=^TestHelperProcess$"}

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
	forgedClient := NewClient(session.socket, "forged")
	defer forgedClient.http.CloseIdleConnections()
	forged := forgedClient.Record(t.Context(), Receipt{Key: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64)})
	if forged == nil || len(store.receipts) != 2 {
		t.Fatalf("record without the session token = %v, receipts %d", forged, len(store.receipts))
	}
	if got := store.receipts[0].Package; got != "pkg" {
		t.Fatalf("package = %q, want pkg", got)
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
	env := Environment{Root: "/work/repo", TempDirs: []string{scratch, "/work/repo/tmp"}, IgnoreDirs: []string{"/dev/null"}, UncacheableDirs: []string{"/dev", "/proc"}}
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
		{name: "impossible paths are not inputs", log: "# test log\nopen bad\x00name\n", want: []string{}},
		{name: "system reads are uncacheable", log: "# test log\nopen /proc/cpuinfo\n", wantErr: true},
		{name: "a stat of the device directory itself is an input", log: "# test log\nstat /dev\n", want: []string{"stat\ta\t/dev"}},
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
		value, err := Digest(inputs, Environment{Root: root, LookupEnv: func(string) (string, bool) { return workspace, true }})
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
	if _, err := Digest([]string{"open\tm\t../escape"}, Environment{Root: first}); err == nil {
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
	getenv := func(string) string { return "" }
	joined, err := Key(binary, []string{"-test.run=^A$\narg 13 -test.skip=B"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	split, err := Key(binary, []string{"-test.run=^A$", "-test.skip=B"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := Key(binary, []string{"-test.run=^A$", "-test.skip=B"}, func(name string) string {
		if name == "GOMAXPROCS" {
			return "1"
		}
		return ""
	})
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
