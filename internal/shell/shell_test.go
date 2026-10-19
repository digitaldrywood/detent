package shell

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestCommandSpecUsesPerOSDefaults(t *testing.T) {
	tests := []struct {
		name          string
		goos          string
		gitDir        string
		bash          bool
		bashDirectory bool
		wsl           bool
		shell         string
		wantName      string
		wantArgs      []string
	}{
		{name: "linux", goos: "linux", gitDir: "cmd", bash: true, wantName: "sh", wantArgs: []string{"-c", "echo ok"}},
		{name: "darwin", goos: "darwin", gitDir: "cmd", bash: true, wantName: "sh", wantArgs: []string{"-c", "echo ok"}},
		{name: "git cmd directory", goos: "windows", gitDir: "cmd", bash: true, wantArgs: []string{"-c", "echo ok"}},
		{name: "git bin directory", goos: "windows", gitDir: "bin", bash: true, wantArgs: []string{"-c", "echo ok"}},
		{name: "git mingw64 directory", goos: "windows", gitDir: "mingw64/bin", bash: true, wantArgs: []string{"-c", "echo ok"}},
		{name: "git mingw32 directory", goos: "windows", gitDir: "mingw32/bin", bash: true, wantArgs: []string{"-c", "echo ok"}},
		{name: "git absent", goos: "windows", wantName: "cmd", wantArgs: []string{"/C", "echo ok"}},
		{name: "only WSL bash", goos: "windows", wsl: true, wantName: "cmd", wantArgs: []string{"/C", "echo ok"}},
		{name: "git without bash and WSL present", goos: "windows", gitDir: "cmd", wsl: true, wantName: "cmd", wantArgs: []string{"/C", "echo ok"}},
		{name: "bash is a directory", goos: "windows", gitDir: "cmd", bashDirectory: true, wantName: "cmd", wantArgs: []string{"/C", "echo ok"}},
		{name: "explicit powershell", goos: "windows", gitDir: "cmd", bash: true, shell: " pwsh ", wantName: "pwsh", wantArgs: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "echo ok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			gitRoot := filepath.Join(root, "Program Files", "Git")
			bash := filepath.Join(gitRoot, "bin", "bash.exe")
			pathDirs := []string{root}
			files := []string{}
			if tt.gitDir != "" {
				dir := filepath.Join(gitRoot, filepath.FromSlash(tt.gitDir))
				gitName := "git"
				if runtime.GOOS == "windows" {
					gitName += ".exe"
				}
				files = append(files, filepath.Join(dir, gitName))
				pathDirs = append(pathDirs, dir)
			}
			if tt.bash {
				files = append(files, bash)
			}
			if tt.bashDirectory {
				if err := os.MkdirAll(bash, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.wsl {
				dir := filepath.Join(root, "Windows", "System32")
				files = append(files, filepath.Join(dir, "bash.exe"))
				pathDirs = append([]string{dir}, pathDirs...)
			}
			for _, file := range files {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, nil, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", strings.Join(pathDirs, string(os.PathListSeparator)))
			t.Setenv("PATHEXT", ".EXE")
			wantName := tt.wantName
			if wantName == "" {
				wantName = bash
			}
			got := CommandSpecForOS("echo ok", tt.shell, tt.goos)
			if got.Name != wantName || !reflect.DeepEqual(got.Args, tt.wantArgs) {
				t.Fatalf("CommandSpecForOS() = %#v, want name %q args %#v", got, wantName, tt.wantArgs)
			}
		})
	}
}

func TestCommandSpecUsesConfiguredShell(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		goos     string
		shell    string
		wantName string
		wantArgs []string
	}{
		{
			name:     "unix custom shell",
			goos:     "linux",
			shell:    "bash",
			wantName: "bash",
			wantArgs: []string{"-c", "echo ok"},
		},
		{
			name:     "windows cmd path",
			goos:     "windows",
			shell:    `C:\Windows\System32\cmd.exe`,
			wantName: `C:\Windows\System32\cmd.exe`,
			wantArgs: []string{"/C", "echo ok"},
		},
		{
			name:     "windows powershell",
			goos:     "windows",
			shell:    "pwsh",
			wantName: "pwsh",
			wantArgs: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "echo ok"},
		},
		{
			name:     "windows posix shell override",
			goos:     "windows",
			shell:    "bash",
			wantName: "bash",
			wantArgs: []string{"-c", "echo ok"},
		},
		{
			name:     "windows posix shell exe name",
			goos:     "windows",
			shell:    "bash.exe",
			wantName: "bash.exe",
			wantArgs: []string{"-c", "echo ok"},
		},
		{
			name:     "windows posix shell exe path",
			goos:     "windows",
			shell:    `C:\Program Files\Git\bin\bash.exe`,
			wantName: `C:\Program Files\Git\bin\bash.exe`,
			wantArgs: []string{"-c", "echo ok"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := CommandSpecForOS("echo ok", tt.shell, tt.goos)
			if got.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if !reflect.DeepEqual(got.Args, tt.wantArgs) {
				t.Fatalf("Args = %#v, want %#v", got.Args, tt.wantArgs)
			}
		})
	}
}

func TestCommandSpecWithArgsQuotesForConfiguredShell(t *testing.T) {
	t.Parallel()

	args := []string{"-p", "--model", "fable", "Bash(git *)", "quoted'value", "percent%value"}
	tests := []struct {
		name     string
		goos     string
		shell    string
		wantName string
		wantArgs []string
	}{
		{
			name:     "unix",
			goos:     "linux",
			wantName: "sh",
			wantArgs: []string{"-c", "claude '-p' '--model' 'fable' 'Bash(git *)' 'quoted'\\''value' 'percent%value'"},
		},
		{
			name:     "windows cmd",
			goos:     "windows",
			wantName: "cmd",
			wantArgs: []string{"/C", `claude ^"-p^" ^"--model^" ^"fable^" ^"Bash^(git^ ^*^)^" ^"quoted'value^" ^"percent^%value^"`},
		},
		{
			name:     "windows powershell",
			goos:     "windows",
			shell:    "pwsh",
			wantName: "pwsh",
			wantArgs: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "claude '-p' '--model' 'fable' 'Bash(git *)' 'quoted''value' 'percent%value'"},
		},
		{
			name:     "windows posix shell",
			goos:     "windows",
			shell:    "bash",
			wantName: "bash",
			wantArgs: []string{"-c", "claude '-p' '--model' 'fable' 'Bash(git *)' 'quoted'\\''value' 'percent%value'"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := CommandSpecWithArgsForOS("claude", tt.shell, args, tt.goos)
			if got.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if !reflect.DeepEqual(got.Args, tt.wantArgs) {
				t.Fatalf("Args = %#v, want %#v", got.Args, tt.wantArgs)
			}
		})
	}
}

func TestCommandBuildsExecCommand(t *testing.T) {
	t.Parallel()

	cmd := CommandForOS(context.Background(), "echo ok", "bash", "linux")
	if !reflect.DeepEqual(cmd.Args, []string{"bash", "-c", "echo ok"}) {
		t.Fatalf("Args = %#v, want bash -c command", cmd.Args)
	}
}

func TestQuoteCmdArg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		arg  string
		want string
	}{
		{name: "empty", want: `^"^"`},
		{name: "spaces", arg: `path with spaces`, want: `^"path^ with^ spaces^"`},
		{name: "embedded quote", arg: `say "hello"`, want: `^"say^ \^"hello\^"^"`},
		{name: "percent", arg: `%PATH%`, want: `^"^%PATH^%^"`},
		{name: "trailing backslash", arg: `C:\path with spaces\`, want: `^"C:\path^ with^ spaces\\^"`},
		{name: "backslash before quote", arg: `a\"b`, want: `^"a\\\^"b^"`},
		{name: "shell syntax", arg: `a&b|c<d>e(f)^!`, want: `^"a^&b^|c^<d^>e^(f^)^^^!^"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := quoteCmdArg(tt.arg); got != tt.want {
				t.Fatalf("quoteCmdArg(%q) = %q, want %q", tt.arg, got, tt.want)
			}
		})
	}
}
