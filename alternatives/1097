package gotestreceipt

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestReceiptInputRegressions(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		temp   bool
		change func(t *testing.T, env *Environment, path string)
	}{
		{name: "temporary stat modification time", input: "stat\tt:TMPDIR\tfixture", temp: true, change: func(t *testing.T, _ *Environment, path string) {
			setTime(t, path, time.Unix(100, 123456789))
		}},
		{name: "checkout file content of the same size", input: "open\tm\tfixture", change: func(t *testing.T, _ *Environment, path string) {
			writeFile(t, path, "fail")
		}},
		{name: "checkout file removed", input: "open\tm\tfixture", change: func(t *testing.T, _ *Environment, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "environment path of a different length", input: "env\tWORKSPACE", change: func(_ *testing.T, env *Environment, _ string) {
			env.Root += "-longer"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "fixture")
			writeFile(t, path, "pass")
			env := Environment{DigestKey: testDigestKey, Root: root}
			if tt.temp {
				env = Environment{DigestKey: testDigestKey, Root: t.TempDir(), TempDirs: []TempRoot{{"TMPDIR", root}}}
			}
			env.LookupEnv = func(string) (string, bool) { return env.Root, true }
			before, err := Digest([]string{tt.input}, env)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, &env, path)
			after, err := Digest([]string{tt.input}, env)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Fatal("changed input reused the same digest")
			}
		})
	}
	t.Run("checkout metadata alone keeps the digest", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "fixture")
		writeFile(t, path, "pass")
		env := Environment{DigestKey: testDigestKey, Root: root}
		inputs := []string{"open\tm\tfixture", "stat\tm\tfixture", "stat\tm\t."}
		before, err := Digest(inputs, env)
		if err != nil {
			t.Fatal(err)
		}
		setTime(t, path, time.Unix(100, 0))
		setTime(t, root, time.Unix(100, 0))
		if after, err := Digest(inputs, env); err != nil || after != before {
			t.Fatalf("a fresh checkout's timestamps changed the digest: %v", err)
		}
	})
	t.Run("deleted temporary input", func(t *testing.T) {
		root := t.TempDir()
		env := Environment{DigestKey: testDigestKey, Root: t.TempDir(), TempDirs: []TempRoot{{"TMPDIR", root}}}
		if _, err := ParseTestLog(strings.NewReader("# test log\nopen "+filepath.Join(root, "fixture")+"\n"), root, env, time.Now()); err == nil {
			t.Fatal("removed temporary input accepted")
		}
	})
	t.Run("temporary root stat", func(t *testing.T) {
		root := t.TempDir()
		env := Environment{DigestKey: testDigestKey, Root: t.TempDir(), TempDirs: []TempRoot{{"TMPDIR", root}}}
		got, err := ParseTestLog(strings.NewReader("# test log\nstat "+root+"\n"), root, env, time.Now())
		if err != nil || len(got) != 1 {
			t.Fatalf("temporary root stat lost: inputs=%v error=%v", got, err)
		}
	})
}

func TestReceiptKeyRegressions(t *testing.T) {
	for _, kind := range []string{"binary argument boundary", "working directory", "runtime environment"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "pkg.test")
			writeFile(t, binary, "binary")
			args := []string{"-test.short=true"}
			workdir := "m\tpkg"
			var environ []string
			before, err := Key(binary, args, workdir, environ)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "binary argument boundary":
				writeFile(t, binary, "binary\narg 16 -test.short=true")
				args = nil
			case "working directory":
				workdir = "m\tother"
			case "runtime environment":
				environ = []string{"GODEBUG=x509sha1=1"}
			}
			after, err := Key(binary, args, workdir, environ)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Fatal("changed execution context reused the same key")
			}
		})
	}
}

func TestWrapperRejectsUnobservedInputs(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		environment  bool
	}{
		{"subprocess", `package fixture
import ("os/exec"; "testing")
func TestInput(t *testing.T) { if err := exec.Command("go", "run", "./child").Run(); err != nil { t.Fatal(err) } }
`, false},
		{"TestMain shadowed package", `package fixture
import ("os"; "testing")
type reader struct{}
func (reader) Getenv(string) string { data, _ := os.ReadFile("input.txt"); return string(data) }
func TestMain(m *testing.M) { failed := false; { os := reader{}; failed = os.Getenv("INPUT") != "pass\n" }; if failed { os.Exit(1) }; os.Exit(m.Run()) }
func TestInput(t *testing.T) {}
`, false},
		{"TestMain shadowed Run receiver", `package fixture
import ("os"; "testing")
type reader struct{}
func (reader) Run() int { data, err := os.ReadFile("input.txt"); if err != nil || string(data) != "pass\n" { return 1 }; return 0 }
func TestMain(m *testing.M) { { m := reader{}; if m.Run() != 0 { os.Exit(1) } }; os.Exit(m.Run()) }
func TestInput(t *testing.T) {}
`, false},
		{"deleted build input", `package fixture
import ("os"; "testing")
func TestInput(t *testing.T) { path := os.Getenv("RECEIPT_BUILD_INPUT"); data, err := os.ReadFile(path); if err != nil || string(data) != "pass\n" { t.Fatal("missing build input") }; if err := os.Remove(path); err != nil { t.Fatal(err) } }
`, false},
		{"TestMain file", `package fixture
import ("os"; "testing")
func TestMain(m *testing.M) { data, err := os.ReadFile("input.txt"); if err != nil || string(data) != "pass\n" { os.Exit(1) }; os.Exit(m.Run()) }
func TestInput(t *testing.T) {}
`, false},
		{"TestMain environment", `package fixture
import ("os"; "testing")
func TestMain(m *testing.M) { if os.Getenv("RECEIPT_INPUT") != "pass" { os.Exit(1) }; os.Exit(m.Run()) }
func TestInput(t *testing.T) {}
`, true},
		{"Environ", `package fixture
import ("os"; "testing"; "slices")
func TestInput(t *testing.T) { if !slices.Contains(os.Environ(), "RECEIPT_INPUT=pass") { t.Fatal("changed environment") } }
`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "child"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "child", "main.go"), []byte(`package main
import "os"
func main() { data, err := os.ReadFile("input.txt"); if err != nil || string(data) != "pass\n" { os.Exit(1) } }
`), 0o600); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"go.mod": "module example.org/fixture\n\ngo 1.26\n", "fixture_test.go": tc.source, "input.txt": "pass\n"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store := &memoryStore{}
			session, err := Start(t.Context(), store, nil, t.TempDir(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := session.Close(t.Context()); err != nil {
					t.Error(err)
				}
			}()
			build := filepath.Join(session.Environment()["GOTMPDIR"], "go-build-fixture")
			binary := filepath.Join(build, "b001", "fixture.test")
			if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.name == "deleted build input" {
				input := filepath.Join(build, "input.txt")
				if err := os.WriteFile(input, []byte("pass\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("RECEIPT_BUILD_INPUT", input)
			}
			cmd := exec.CommandContext(t.Context(), "go", "test", "-c", "-o", binary, ".")
			cmd.Dir = root
			procgroup.Configure(t.Context(), cmd)
			if err := procgroup.Start(cmd); err != nil {
				t.Fatal(err)
			}
			if err := procgroup.Wait(cmd); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			t.Setenv("RECEIPT_INPUT", "pass")
			getenv := func(name string) string {
				if value, ok := session.Environment()[name]; ok {
					return value
				}
				if name == RootEnvironment {
					return root
				}
				return os.Getenv(name)
			}
			var output bytes.Buffer
			args := []string{binary, "-test.run=^TestInput$"}
			if code := RunWrapper(args, getenv, strings.NewReader("pass\n"), &output, &output); code != 0 {
				t.Fatalf("first run = %d: %s", code, &output)
			}
			if tc.environment {
				t.Setenv("RECEIPT_INPUT", "fail")
			} else if tc.name != "deleted build input" {
				if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("fail\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			output.Reset()
			if code := RunWrapper(args, getenv, strings.NewReader("fail\n"), &output, &output); code != 1 {
				t.Fatalf("changed input returned %d, want failure: %s", code, &output)
			}
			if stats := session.Stats(); stats.Hits != 0 || stats.Lookups != 2 {
				t.Fatalf("unobserved input reused or bypassed the session: %+v", stats)
			}
			if !tc.environment && session.Stats().Records != 0 {
				t.Fatal("unobserved input recorded")
			}
		})
	}
}
