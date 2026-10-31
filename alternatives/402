package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCheckDoctorHookShell(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		goos     string
		shell    string
		resolved string
		files    fstest.MapFS
		want     string
		status   doctorStatus
	}{
		{name: "windows default", goos: "windows", resolved: "cmd", want: "cmd", status: doctorOK},
		{name: "declared bash on PATH", goos: "windows", shell: "bash", resolved: `D:\Tools\bash.exe`, want: `D:\Tools\bash.exe`, status: doctorOK},
		{name: "declared bash standard install", goos: "windows", shell: "bash", files: fstest.MapFS{`C:\Program Files\Git\bin\bash.exe`: {}}, want: `C:\Program Files\Git\bin\bash.exe`, status: doctorOK},
		{name: "declared bash missing", goos: "windows", shell: "bash", want: `hook_shell_unavailable: shell "bash"`, status: doctorFail},
		{name: "declared sh missing", goos: "windows", shell: "sh", want: `hook_shell_unavailable: shell "sh"`, status: doctorFail},
		{name: "explicit pwsh", goos: "windows", shell: "pwsh", resolved: "pwsh", want: "pwsh", status: doctorOK},
		{name: "darwin", goos: "darwin", resolved: "/bin/sh", want: "/bin/sh", status: doctorOK},
		{name: "linux", goos: "linux", resolved: "/bin/sh", want: "/bin/sh", status: doctorOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps := doctorDeps{
				resolveCommandOnPath: func(checkedPath string, binary string) (string, error) {
					if checkedPath != "checked-path" {
						t.Fatalf("PATH = %q", checkedPath)
					}
					if tt.resolved == "" {
						return "", exec.ErrNotFound
					}
					return tt.resolved, nil
				},
				statHookShell: func(name string) (os.FileInfo, error) { return tt.files.Stat(name) },
			}
			got := checkDoctorHookShell(tt.shell, tt.goos, deps, doctorBinaryEnvironment{CheckedPath: "checked-path"})
			if got.Name != "Hook shell" || got.Status != tt.status || !strings.HasPrefix(got.Detail, tt.want) {
				t.Fatalf("checkDoctorHookShell() = %#v, want status %q shell %q", got, tt.status, tt.want)
			}
		})
	}
}
