package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolveWithReaderFallsBackToVCSBuildSettings(t *testing.T) {
	t.Parallel()

	info := ResolveWithReader("dev", "none", "unknown", func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abcdef1234567890"},
				{Key: "vcs.time", Value: "2026-06-05T21:00:00Z"},
				{Key: "vcs.modified", Value: "true"},
			},
		}, true
	})

	if info.Version != "dev" {
		t.Fatalf("Version = %q, want dev", info.Version)
	}
	if info.Commit != "abcdef1234567890" {
		t.Fatalf("Commit = %q, want abcdef1234567890", info.Commit)
	}
	if info.Date != "2026-06-05T21:00:00Z" {
		t.Fatalf("Date = %q, want 2026-06-05T21:00:00Z", info.Date)
	}
	if !info.Dirty {
		t.Fatal("Dirty = false, want true")
	}
}

func TestResolveWithReadersFallsBackToGitWhenVCSBuildSettingsAreMissing(t *testing.T) {
	t.Parallel()

	info := ResolveWithReaders("dev", "none", "unknown", func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Path: "github.com/digitaldrywood/detent"},
		}, true
	}, func(modulePath string) (Info, bool) {
		if modulePath != "github.com/digitaldrywood/detent" {
			t.Fatalf("modulePath = %q, want github.com/digitaldrywood/detent", modulePath)
		}
		return Info{
			Commit: "82d5622",
			Date:   "2026-06-05T12:53:11Z",
			Dirty:  true,
		}, true
	})

	if info.Commit != "82d5622" {
		t.Fatalf("Commit = %q, want 82d5622", info.Commit)
	}
	if info.Date != "2026-06-05T12:53:11Z" {
		t.Fatalf("Date = %q, want 2026-06-05T12:53:11Z", info.Date)
	}
	if !info.Dirty {
		t.Fatal("Dirty = false, want true")
	}
}

func TestDisplayLabelFormatsShortCommitAndDirtyMarker(t *testing.T) {
	t.Parallel()

	info := Info{
		Version: "dev",
		Commit:  "abcdef1234567890",
		Date:    "2026-06-05T21:00:00Z",
		Dirty:   true,
	}

	if got, want := DisplayLabel(info), "dev (abcdef1, dirty) 2026-06-05T21:00:00Z"; got != want {
		t.Fatalf("DisplayLabel() = %q, want %q", got, want)
	}
}
