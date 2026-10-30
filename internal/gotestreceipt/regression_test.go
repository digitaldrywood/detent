package gotestreceipt

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestReceiptInputRegressions(t *testing.T) {
	for _, kind := range []string{"file stat mtime", "open file mtime", "directory stat mtime", "open file identity", "deleted fixture", "deleted temporary input", "literal environment path", "temporary root stat"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "fixture")
			if kind == "directory stat mtime" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("pass"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := Environment{Root: root}
			inputs := []string{"stat\tm\tfixture"}
			if kind == "open file mtime" || kind == "open file identity" {
				inputs = []string{"open\tm\tfixture"}
			}
			switch kind {
			case "temporary root stat":
				env.TempDirs = []string{root}
				got, err := ParseTestLog(strings.NewReader("# test log\nstat "+root+"\n"), root, env, time.Now())
				if err != nil || len(got) != 1 {
					t.Fatalf("temporary root stat lost: inputs=%v error=%v", got, err)
				}
				return
			case "deleted temporary input":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				env.TempDirs = []string{root}
				if _, err := ParseTestLog(strings.NewReader("# test log\nopen "+path+"\n"), root, env, time.Now()); err == nil {
					t.Fatal("removed temporary input accepted")
				}
				return
			case "deleted fixture":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if _, err := Digest(inputs, env); err == nil {
					t.Fatal("removed input accepted")
				}
				return
			case "literal environment path":
				inputs = []string{"env\tWORKSPACE"}
				env.LookupEnv = func(string) (string, bool) { return root, true }
			}
			before, err := Digest(inputs, env)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "open file identity" {
				replacement := filepath.Join(root, "replacement")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(replacement, []byte("pass"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "literal environment path" {
				env.Root = root + "-longer"
				env.LookupEnv = func(string) (string, bool) { return env.Root, true }
			} else {
				stamp := time.Unix(100, 123456789)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			after, err := Digest(inputs, env)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Fatal("changed input reused the same digest")
			}
		})
	}
}

func TestReceiptKeyRegressions(t *testing.T) {
	for _, kind := range []string{"binary argument boundary", "working directory", "checkout root", "environment outside log", "environment order"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, "pkg.test")
			if err := os.WriteFile(binary, []byte("binary"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.short=true"}
			getenv := func(string) string { return "" }
			if kind == "environment order" {
				t.Setenv("RECEIPT_ENV_FIRST", "first")
				t.Setenv("RECEIPT_ENV_SECOND", "second")
			}
			before, err := Key(binary, args, getenv)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "binary argument boundary":
				if err := os.WriteFile(binary, []byte("binary\narg 16 -test.short=true"), 0o600); err != nil {
					t.Fatal(err)
				}
				args = nil
			case "working directory":
				t.Chdir(dir)
			case "checkout root":
				getenv = func(name string) string {
					if name == RootEnvironment {
						return dir
					}
					return ""
				}
			case "environment order":
				if err := os.Unsetenv("RECEIPT_ENV_FIRST"); err != nil {
					t.Fatal(err)
				}
				if err := os.Setenv("RECEIPT_ENV_FIRST", "first"); err != nil {
					t.Fatal(err)
				}
			case "environment outside log":
				t.Setenv("RECEIPT_PRE_MAIN_INPUT", "changed")
			}
			after, err := Key(binary, args, getenv)
			if err != nil {
				t.Fatal(err)
			}
			if before == after {
				t.Fatal("changed execution context reused the same key")
			}
		})
	}
}

func TestReceiptEligibilityRegressions(t *testing.T) {
	cases := []struct {
		name, source string
		refused      bool
	}{
		{"subprocess inputs", `package fixture; import "os/exec"; func run() { exec.Command("bash", "fixture.sh").Run() }`, true},
		{"subprocess alias", `package fixture; import child "os/exec"; func run() { child.Command("bash", "fixture.sh").Run() }`, true},
		{"TestMain reads outside log", `package fixture; import ("os"; "testing"); func TestMain(m *testing.M) { os.ReadFile("fixture"); os.Exit(m.Run()) }`, true},
		{"TestMain environment captured by key", `package fixture; import ("os"; "testing"); func TestMain(m *testing.M) { if os.Getenv("INPUT") == "bad" { os.Exit(1) }; os.Exit(m.Run()) }`, false},
		{"init environment captured by key", `package fixture; import "os"; var input = os.Getenv("INPUT"); func init() { os.Setenv("OTHER", input) }`, false},
		{"TestMain shadowed environment reader", `package fixture; import ("os"; "testing"); type reader struct{}; func (reader) Getenv(string) string { data, _ := os.ReadFile("fixture"); return string(data) }; func TestMain(m *testing.M) { os := reader{}; os.Getenv("INPUT"); m.Run() }`, true},
		{"initializer shadowed pure package", `package fixture; import ("os"; "reflect"); type reader struct{}; func (reader) TypeOf(string) string { data, _ := os.ReadFile("fixture"); return string(data) }; func init() { reflect := reader{}; reflect.TypeOf("INPUT") }`, true},
		{"TestMain indirect input", `package fixture; import "testing"; func TestMain(m *testing.M) { setup(); m.Run() }`, true},
		{"TestMain disguised run", `package fixture; func TestMain(m *fixture) { m.Run() }`, true},
		{"init reads outside log", `package fixture; import "os"; func init() { os.ReadFile("fixture") }`, true},
		{"initializer reads outside log", `package fixture; import "os"; var input, err = os.ReadFile("fixture")`, true},
		{"pure initializer", `package fixture; import "errors"; var Err = errors.New("fixture")`, false},
		{"reflection type initializer", `package fixture; import ("reflect"; "time"); var durationType = reflect.TypeOf(time.Duration(0))`, false},
		{"reflection environment initializer captured by key", `package fixture; import ("reflect"; "os"); var envType = reflect.TypeOf(os.Getenv("INPUT"))`, false},
		{"constant time comparison", `package fixture; import "crypto/subtle"; func equal(a, b []byte) int { return subtle.ConstantTimeCompare(a, b) }`, false},
		{"nested environment initializer captured by key", `package fixture; import ("errors"; "os"); var Err = errors.New(os.Getenv("INPUT"))`, false},
		{"embedded input", `package fixture; import _ "embed"; var data string`, false},
		{"syntax tree", `package fixture; import "go/ast"; func tree() ast.Node { return &ast.Ident{Name: "fixture"} }`, false},
		{"token position", `package fixture; import "go/token"; var pos token.Pos`, false},
		{"memory filesystem", `package fixture; import "testing/fstest"; var files fstest.MapFS`, false},
		{"pure hash", `package fixture; import "crypto/sha256"; func hash(p []byte) [32]byte { return sha256.Sum256(p) }`, false},
		{"network service", `package fixture; import "net/http"; func run() { http.Get("http://example.invalid") }`, true},
		{"clock", `package fixture; import "time"; func run() { time.Now() }`, true},
		{"descriptor metadata", `package fixture; import "os"; func run() { os.Stdout.Stat() }`, true},
		{"timezone file", `package fixture; import "time"; func run() { time.Parse("MST", "UTC") }`, true},
		{"implicit local timezone", `package fixture; import "time"; func run(t time.Time) { t.Local() }`, true},
		{"kernel state", `package fixture; import "syscall"; func run() { syscall.Getpid() }`, true},
		{"hermetic duration", `package fixture; import "time"; const interval = time.Second`, false},
		{"environment captured by key", `package fixture; import "os"; func run() { os.Environ() }`, false},
		{"logged file", `package fixture; import "os"; func run() { var data, err = os.ReadFile("fixture"); _, _ = data, err }`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", tc.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			err = checkSource(file)
			if (err != nil) != tc.refused {
				t.Fatalf("eligibility error = %v, refused = %v", err, tc.refused)
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
			binary := filepath.Join(root, "fixture.test")
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
			store := &memoryStore{}
			session, err := Start(t.Context(), store, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := session.Close(t.Context()); err != nil {
					t.Error(err)
				}
			}()
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
			if code := RunWrapper(args, getenv, nil, &output, &output); code != 0 {
				t.Fatalf("first run = %d: %s", code, &output)
			}
			if tc.environment {
				t.Setenv("RECEIPT_INPUT", "fail")
			} else if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("fail\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if code := RunWrapper(args, getenv, nil, &output, &output); code != 1 {
				t.Fatalf("changed input returned %d, want failure: %s", code, &output)
			}
			if stats := session.Stats(); stats.Hits != 0 {
				t.Fatalf("unobserved input reused: %+v", stats)
			}
			if !tc.environment && session.Stats().Records != 0 {
				t.Fatal("unobserved input recorded")
			}
		})
	}
}
