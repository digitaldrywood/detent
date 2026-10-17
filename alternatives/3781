package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateRanges(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Example")
	t.Setenv("GIT_COMMITTER_NAME", "Example")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.test")
	t.Setenv("GIT_COMMITTER_EMAIL", "author@example.test")
	run := func(args ...string) string {
		t.Helper()
		raw, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, raw)
		}
		return string(raw)
	}
	run("init", "-b", "develop")
	run("config", "commit.gpgsign", "false")
	run("config", "tag.gpgsign", "false")
	commit := func(at, subject, body string) {
		t.Helper()
		t.Setenv("GIT_AUTHOR_DATE", at)
		t.Setenv("GIT_COMMITTER_DATE", at)
		run("-c", "core.hooksPath="+filepath.Join(t.TempDir(), "hooks"), "commit", "--allow-empty", "-m", subject, "-m", body)
	}
	commit("2026-10-13T01:00:00Z", "feat: initial feed (#101)", "")
	run("tag", "-a", "v1.2.0", "-m", "release")
	commit("2026-10-14T01:00:00Z", "fix: complete prj_example#12 (#102)", "fix(hub): retain stale pull refs")
	run("tag", "-a", "v1.2.0-op.r.1.g123456789abc", "-m", "patch")
	commit("2026-10-14T02:00:00Z", "fix: complete prj_example#13 (#103)", "")
	for i, kind := range []string{"test", "ci", "chore", "docs", "build"} {
		commit("2026-10-14T02:00:00Z", kind+": internal work", string(rune('a'+i)))
	}
	commit("2026-10-14T02:00:00Z", "feat: initial feed (#101) (#201)", "")
	at := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	builds, err := generate("v1.2.1", "HEAD", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 3 {
		t.Fatalf("builds=%+v", builds)
	}
	for i, want := range []struct {
		version      string
		pr, internal int
		patch        bool
	}{
		{"v1.2.1", 103, 5, false}, {"v1.2.0-op.r.1.g123456789abc", 102, 0, true}, {"v1.2.0", 101, 0, false},
	} {
		got := builds[i]
		if got.Version != want.version || got.Patch != want.patch || len(got.Entries) != 1 || got.Entries[0].PRNumber != want.pr || got.InternalCount != want.internal {
			t.Fatalf("build %d=%+v", i, got)
		}
	}
	if builds[0].Entries[0].Text != "Maintenance update" || builds[1].Entries[0].Text != "Retain stale pull refs" || !builds[0].BuildTime.Equal(at) {
		t.Fatalf("builds=%+v", builds)
	}
	older, err := generate("v1.2.0", "v1.2.0", at.Add(-26*time.Hour))
	if err != nil || len(older) != 1 {
		t.Fatalf("future tags included: %+v %v", older, err)
	}
}
