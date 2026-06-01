package shell

import (
	"context"
	"reflect"
	"testing"
)

func TestCommandSpecUsesPerOSDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		goos     string
		command  string
		wantName string
		wantArgs []string
	}{
		{
			name:     "unix",
			goos:     "linux",
			command:  "printf ok",
			wantName: "sh",
			wantArgs: []string{"-c", "printf ok"},
		},
		{
			name:     "windows",
			goos:     "windows",
			command:  "echo ok",
			wantName: "cmd",
			wantArgs: []string{"/C", "echo ok"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := CommandSpecForOS(tt.command, "", tt.goos)
			if got.Name != tt.wantName {
				t.Fatalf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if !reflect.DeepEqual(got.Args, tt.wantArgs) {
				t.Fatalf("Args = %#v, want %#v", got.Args, tt.wantArgs)
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

func TestCommandBuildsExecCommand(t *testing.T) {
	t.Parallel()

	cmd := CommandForOS(context.Background(), "echo ok", "bash", "linux")
	if !reflect.DeepEqual(cmd.Args, []string{"bash", "-c", "echo ok"}) {
		t.Fatalf("Args = %#v, want bash -c command", cmd.Args)
	}
}
