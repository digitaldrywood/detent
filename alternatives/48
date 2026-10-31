package main

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestFormatVersionInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info versionInfo
		want string
	}{
		{
			name: "development build",
			info: versionInfo{
				Version:   "dev",
				Commit:    "none",
				Date:      "unknown",
				GoVersion: "go1.26.0",
				OS:        "linux",
				Arch:      "amd64",
			},
			want: "version: dev\ncommit: none\nbuild date: unknown\ngo version: go1.26.0\nos/arch: linux/amd64\n",
		},
		{
			name: "release build",
			info: versionInfo{
				Version:   "v1.2.3",
				Commit:    "abc1234",
				Date:      "2026-05-31T18:30:00Z",
				GoVersion: "go1.26.1",
				OS:        "darwin",
				Arch:      "arm64",
			},
			want: "version: v1.2.3\ncommit: abc1234\nbuild date: 2026-05-31T18:30:00Z\ngo version: go1.26.1\nos/arch: darwin/arm64\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := formatVersionInfo(tt.info); got != tt.want {
				t.Fatalf("formatVersionInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionCommandPrintsBuildFields(t *testing.T) {
	withVersionMetadata(t, "v1.2.3", "abc1234", "2026-05-31T18:30:00Z")

	cmd := newRootCommand(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "pretty", "version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"version: v1.2.3",
		"commit: abc1234",
		"build date: 2026-05-31T18:30:00Z",
		"go version: " + runtime.Version(),
		"os/arch: " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("version output missing %q:\n%s", want, output)
		}
	}
}

func TestVersionCommandDefaultsJSONForNonTTY(t *testing.T) {
	withVersionMetadata(t, "v1.2.3", "abc1234", "2026-05-31T18:30:00Z")

	cmd := newRootCommand(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var got struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildDate string `json:"build_date"`
		GoVersion string `json:"go_version"`
		OS        string `json:"os"`
		Arch      string `json:"arch"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("version output is not JSON: %v\n%s", err, stdout.String())
	}
	if got.Version != "v1.2.3" || got.Commit != "abc1234" || got.BuildDate != "2026-05-31T18:30:00Z" {
		t.Fatalf("version metadata = %#v, want build metadata", got)
	}
	if got.GoVersion != runtime.Version() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("runtime metadata = %#v, want current runtime", got)
	}
}

func TestRootVersionFlagPrintsVersionString(t *testing.T) {
	withVersionMetadata(t, "v1.2.3", "abc1234", "2026-05-31T18:30:00Z")

	cmd := newRootCommand(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := stdout.String(), "v1.2.3\n"; got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}
}

func withVersionMetadata(t *testing.T, nextVersion, nextCommit, nextDate string) {
	t.Helper()

	oldVersion := version
	oldCommit := commit
	oldDate := date
	version = nextVersion
	commit = nextCommit
	date = nextDate

	t.Cleanup(func() {
		version = oldVersion
		commit = oldCommit
		date = oldDate
	})
}
