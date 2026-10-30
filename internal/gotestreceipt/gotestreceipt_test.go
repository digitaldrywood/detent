package gotestreceipt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	helperEnvironment = "GOTESTRECEIPT_HELPER"
	helperRan         = "gotestreceipt helper ran"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnvironment) == "" {
		t.Skip("helper process only")
	}
	fmt.Println(helperRan)
	for _, name := range []string{SocketEnvironment, TokenEnvironment, RootEnvironment} {
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
	pkg := filepath.Join(root, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(pkg, "input.txt")
	t.Setenv(helperEnvironment, "1")
	t.Setenv("GOCOVERDIR", t.TempDir())
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{executable, "-test.paniconexit0", "-test.run=^TestHelperProcess$"}

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
		{name: "restored input reuses receipt", content: "pass"},
		{name: "changed environment executes", content: "pass", mode: "fail", wantCode: 1, wantRun: true},
		{name: "a test that rewrites its input executes and is not recorded", content: "pass", mode: "rewrite", wantRun: true},
	}
	for _, step := range steps {
		if err := os.WriteFile(input, []byte(step.content), 0o600); err != nil {
			t.Fatal(err)
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
	forged := NewClient(session.socket, "forged").Record(t.Context(), Receipt{Key: strings.Repeat("a", 64), Digest: strings.Repeat("b", 64)})
	if forged == nil || len(store.receipts) != 1 {
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
			name: "temporary roots, removed temporary files and device paths are not inputs",
			log:  "# test log\nopen " + scratch + "\nopen " + filepath.Join(scratch, "TestX", "001", "db") + "\nopen /dev/null\nopen /etc/hosts\n",
			want: []string{"open\ta\t/etc/hosts"},
		},
		{
			name: "temporary files that predate the test are absolute inputs and files it wrote are not",
			log:  "# test log\nopen " + kept + "\nopen " + written + "\nopen /work/repo/tmp/config\n",
			want: []string{"open\ta\t" + kept},
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

func TestDigestIgnoresCheckoutLocation(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, root := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("same"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputs := []string{"open\tm\tdata.txt", "stat\tm\tmissing", "env\tDETENT_WORKSPACE"}
	digest := func(root, workspace string) string {
		t.Helper()
		value, err := Digest(inputs, Environment{Root: root, LookupEnv: func(string) (string, bool) { return workspace, true }})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if digest(first, first) != digest(second, second) {
		t.Fatal("digest differs between checkouts with identical inputs")
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
	inputs := []string{"open\tm\tinput.txt", "stat\tm\tmissing", "stat\tm\t.", "env\tHOME"}
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
