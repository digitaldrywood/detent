package shell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCommandSpecUsesPerOSDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		goos        string
		shell       string
		path        map[string]string
		files       fstest.MapFS
		wantName    string
		wantArgs    []string
		wantMissing string
	}{
		{name: "linux default", goos: "linux", path: map[string]string{"sh": "/bin/sh"}, wantName: "/bin/sh", wantArgs: []string{"-c", "echo ok"}},
		{name: "darwin default", goos: "darwin", path: map[string]string{"sh": "/bin/sh"}, wantName: "/bin/sh", wantArgs: []string{"-c", "echo ok"}},
		{name: "windows default with Git Bash installed", goos: "windows", path: map[string]string{"cmd": `C:\Windows\System32\cmd.exe`}, files: fstest.MapFS{`C:\Program Files\Git\bin\bash.exe`: {}}, wantName: `C:\Windows\System32\cmd.exe`, wantArgs: []string{"/C", "echo ok"}},
		{name: "windows default without Git Bash", goos: "windows", path: map[string]string{"cmd": "cmd"}, wantName: "cmd", wantArgs: []string{"/C", "echo ok"}},
		{name: "declared bash PATH first", goos: "windows", shell: "bash", path: map[string]string{"bash": `D:\Tools\bash.exe`}, files: fstest.MapFS{`C:\Program Files\Git\bin\bash.exe`: {}}, wantName: `D:\Tools\bash.exe`, wantArgs: []string{"-c", "echo ok"}},
		{name: "declared bash standard Git install", goos: "windows", shell: "bash", files: fstest.MapFS{`C:\Program Files\Git\bin\bash.exe`: {}}, wantName: `C:\Program Files\Git\bin\bash.exe`, wantArgs: []string{"-c", "echo ok"}},
		{name: "declared sh standard Git install", goos: "windows", shell: "sh", files: fstest.MapFS{`C:\Program Files\Git\bin\sh.exe`: {}}, wantName: `C:\Program Files\Git\bin\sh.exe`, wantArgs: []string{"-c", "echo ok"}},
		{name: "declared bash missing", goos: "windows", shell: "bash", wantMissing: "bash"},
		{name: "declared sh missing", goos: "windows", shell: "sh", wantMissing: "sh"},
		{name: "bash is a directory", goos: "windows", shell: "bash", files: fstest.MapFS{`C:\Program Files\Git\bin\bash.exe`: {Mode: os.ModeDir}}, wantMissing: "bash"},
		{name: "declared POSIX shell missing", goos: "linux", shell: "bash", wantMissing: "bash"},
		{name: "explicit path does not fall back", goos: "windows", shell: `D:\Tools\bash.exe`, files: fstest.MapFS{`C:\Program Files\Git\bin\bash.exe`: {}}, wantMissing: `D:\Tools\bash.exe`},
		{name: "explicit cmd", goos: "windows", shell: "cmd", path: map[string]string{"cmd": "cmd"}, wantName: "cmd", wantArgs: []string{"/C", "echo ok"}},
		{name: "explicit PowerShell", goos: "windows", shell: " pwsh ", path: map[string]string{"pwsh": "pwsh"}, wantName: "pwsh", wantArgs: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "echo ok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lookPath := func(name string) (string, error) {
				if resolved, ok := tt.path[name]; ok {
					return resolved, nil
				}
				return "", exec.ErrNotFound
			}
			got, err := resolveCommandSpecForOS("echo ok", tt.shell, tt.goos, lookPath, tt.files.Stat)
			if tt.wantMissing != "" {
				var unavailable *UnavailableError
				if !errors.As(err, &unavailable) || unavailable.Shell != tt.wantMissing || !strings.Contains(err.Error(), "hook_shell_unavailable") {
					t.Fatalf("error = %v, want unavailable shell %q", err, tt.wantMissing)
				}
				return
			}
			if err != nil || got.Name != tt.wantName || !reflect.DeepEqual(got.Args, tt.wantArgs) {
				t.Fatalf("resolved command = %#v, %v, want name %q args %#v", got, err, tt.wantName, tt.wantArgs)
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
			shell:    "cmd",
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
	if filepath.Base(cmd.Args[0]) != "bash" && filepath.Base(cmd.Args[0]) != "bash.exe" || !reflect.DeepEqual(cmd.Args[1:], []string{"-c", "echo ok"}) {
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
