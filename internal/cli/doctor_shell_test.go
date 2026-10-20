package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckDoctorHookShell(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		git   bool
		bash  bool
		wsl   bool
		shell string
		want  string
	}{
		{name: "Git Bash present", goos: "windows", git: true, bash: true},
		{name: "only WSL present", goos: "windows", wsl: true, want: "cmd"},
		{name: "Git absent", goos: "windows", want: "cmd"},
		{name: "Git without Bash", goos: "windows", git: true, wsl: true, want: "cmd"},
		{name: "explicit pwsh", goos: "windows", git: true, bash: true, shell: "pwsh", want: "pwsh"},
		{name: "darwin", goos: "darwin", git: true, bash: true, want: "sh"},
		{name: "linux", goos: "linux", git: true, bash: true, want: "sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			gitDir := filepath.Join(root, "Program Files", "Git", "cmd")
			bash := filepath.Join(root, "Program Files", "Git", "bin", "bash.exe")
			wslDir := filepath.Join(root, "Windows", "System32")
			files := []string{}
			if tt.git {
				gitName := "git"
				if runtime.GOOS == "windows" {
					gitName += ".exe"
				}
				files = append(files, filepath.Join(gitDir, gitName))
			}
			if tt.bash {
				files = append(files, bash)
			}
			if tt.wsl {
				files = append(files, filepath.Join(wslDir, "bash.exe"))
			}
			for _, file := range files {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, nil, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pathValue := strings.Join([]string{wslDir, gitDir}, string(os.PathListSeparator))
			t.Setenv("PATH", pathValue)
			t.Setenv("PATHEXT", ".EXE")
			deps := doctorDeps{
				resolveCommandOnPath: func(checkedPath string, binary string) (string, error) {
					if checkedPath != pathValue {
						t.Fatalf("PATH = %q, want %q", checkedPath, pathValue)
					}
					return exec.LookPath(binary)
				},
			}
			got := checkDoctorHookShell(tt.shell, tt.goos, deps, doctorBinaryEnvironment{CheckedPath: pathValue})
			want := tt.want
			if want == "" {
				want = bash
			}
			if got.Name != "Hook shell" || got.Status != doctorOK || !strings.HasPrefix(got.Detail, want+"; ") {
				t.Fatalf("checkDoctorHookShell() = %#v, want OK Hook shell %q", got, want)
			}
		})
	}
}
