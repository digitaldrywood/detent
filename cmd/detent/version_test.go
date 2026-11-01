package main

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
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
		HubSchemaVersion      int64  `json:"hub_schema_version"`
		HubSchemaFingerprint  string `json:"hub_schema_fingerprint"`
		RegistrySchemaVersion int64  `json:"registry_schema_version"`
		RegistryFingerprint   string `json:"registry_schema_fingerprint"`
		AuthSchemaVersion     int64  `json:"auth_schema_version"`
		AuthFingerprint       string `json:"auth_schema_fingerprint"`
		PlatformSchemaVersion int64  `json:"platform_schema_version"`
		PlatformFingerprint   string `json:"platform_schema_fingerprint"`
		Version               string `json:"version"`
		Commit                string `json:"commit"`
		BuildDate             string `json:"build_date"`
		GoVersion             string `json:"go_version"`
		OS                    string `json:"os"`
		Arch                  string `json:"arch"`
		SocketActivation      bool   `json:"socket_activation_supported"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("version output is not JSON: %v\n%s", err, stdout.String())
	}
	latest, err := hubserver.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got.HubSchemaVersion != latest || latest == 0 {
		t.Fatalf("hub schema version = %d, want %d", got.HubSchemaVersion, latest)
	}
	if got.Version != "v1.2.3" || got.Commit != "abc1234" || got.BuildDate != "2026-05-31T18:30:00Z" {
		t.Fatalf("version metadata = %#v, want build metadata", got)
	}
	if got.GoVersion != runtime.Version() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("runtime metadata = %#v, want current runtime", got)
	}
	if !got.SocketActivation {
		t.Fatal("socket activation support = false, want true")
	}
	if got.RegistrySchemaVersion < 1 || got.AuthSchemaVersion < 1 || got.PlatformSchemaVersion < 1 {
		t.Fatalf("entry schema versions = %d/%d/%d, want positive versions", got.RegistrySchemaVersion, got.AuthSchemaVersion, got.PlatformSchemaVersion)
	}
	for name, fingerprint := range map[string]string{"hub": got.HubSchemaFingerprint, "registry": got.RegistryFingerprint, "auth": got.AuthFingerprint, "platform": got.PlatformFingerprint} {
		if len(fingerprint) != 64 {
			t.Fatalf("%s schema fingerprint length = %d, want 64", name, len(fingerprint))
		}
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
