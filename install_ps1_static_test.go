//go:build !windows

package detent_test

import (
	"os"
	"strings"
	"testing"
)

func TestWindowsInstallDocsExposeOneStepPowerShellInstall(t *testing.T) {
	t.Parallel()

	read := func(path string) string {
		t.Helper()

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		return string(raw)
	}

	readme := read("README.md")
	script := read("install.ps1")

	wantCommand := "irm https://raw.githubusercontent.com/digitaldrywood/detent/main/install.ps1 | iex"
	for _, tt := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "readme one-step command", content: readme, want: wantCommand},
		{name: "readme windows path", content: readme, want: `%LOCALAPPDATA%\detent\bin`},
		{name: "script downloads release archive", content: script, want: "releases/latest"},
		{name: "script verifies checksum", content: script, want: "Security.Cryptography.SHA256"},
		{name: "script installs exe", content: script, want: "detent.exe"},
		{name: "script checks os architecture", content: script, want: "OSArchitecture"},
		{name: "script checks 64-bit host from 32-bit powershell", content: script, want: "PROCESSOR_ARCHITEW6432"},
		{name: "script queries cim processor architecture", content: script, want: "Win32_Processor"},
		{name: "script queries cim os architecture", content: script, want: "Get-CimInstance"},
		{name: "script updates user path", content: script, want: "SetEnvironmentVariable('Path'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if !strings.Contains(tt.content, tt.want) {
				t.Fatalf("%s missing %q", tt.name, tt.want)
			}
		})
	}
}

func TestWindowsInstallScriptDoesNotMapGeneric64BitCIMOutputToAmd64(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("install.ps1")
	if err != nil {
		t.Fatalf("ReadFile(install.ps1) error = %v", err)
	}

	script := string(raw)
	for _, disallowed := range []string{
		"'64-bit' { return 'amd64' }",
		`"64-bit" { return "amd64" }`,
	} {
		if strings.Contains(script, disallowed) {
			t.Fatalf("install.ps1 maps generic CIM bitness %q to amd64", disallowed)
		}
	}
}
