package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type protectedThreads struct {
	ids []string
	err error
}

func (p protectedThreads) ProtectedCodexThreadIDs(context.Context) ([]string, error) {
	return p.ids, p.err
}

func TestWorkerTranscriptDirectories(t *testing.T) {
	for _, launchd := range []bool{false, true} {
		for _, migration := range []bool{false, true} {
			name := "worker"
			if launchd {
				name = "launchd"
			}
			if migration {
				name += "/migration"
			}
			t.Run(name, func(t *testing.T) {
				home := t.TempDir()
				source := filepath.Join(home, ".codex")
				profileName := workerCodexProfileDir
				if launchd {
					profileName = launchdCodexProfileDir
				}
				profile := filepath.Join(source, profileName)
				if err := ensureLaunchdCodexProfile(profile); err != nil {
					t.Fatal(err)
				}
				for _, dir := range []string{"sessions", "archived_sessions"} {
					host := filepath.Join(source, dir)
					if err := os.MkdirAll(host, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(host, "personal"), []byte("untouched"), 0600); err != nil {
						t.Fatal(err)
					}
					if migration {
						if err := os.Symlink(host, filepath.Join(profile, dir)); err != nil {
							t.Fatal(err)
						}
					}
				}
				for range 2 {
					if _, _, err := prepareWorkerCodexHome(source, home, launchd); err != nil {
						t.Fatal(err)
					}
					for _, dir := range []string{"sessions", "archived_sessions"} {
						info, err := os.Lstat(filepath.Join(profile, dir))
						if err != nil || !info.IsDir() {
							t.Fatalf("directory %s = %v, %v", dir, info, err)
						}
						content, err := os.ReadFile(filepath.Join(source, dir, "personal"))
						if err != nil || string(content) != "untouched" {
							t.Fatalf("host changed: %q %v", content, err)
						}
						if err := os.WriteFile(filepath.Join(profile, dir, "worker"), []byte("local"), 0600); err != nil {
							t.Fatal(err)
						}
						if _, err := os.Stat(filepath.Join(source, dir, "worker")); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("worker leaked to host: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestPruneCodexTranscripts(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		age       time.Duration
		protected []string
		parent    string
		keep      bool
	}{
		{name: "expired", age: 31 * 24 * time.Hour},
		{name: "recent", age: 29 * 24 * time.Hour, keep: true},
		{name: "boundary", age: 30 * 24 * time.Hour, keep: true},
		{name: "unfinished", age: 31 * 24 * time.Hour, protected: []string{"thread"}, keep: true},
		{name: "resume source", age: 31 * 24 * time.Hour, protected: []string{"thread"}, keep: true},
		{name: "child", age: 31 * 24 * time.Hour, parent: "parent", protected: []string{"parent"}, keep: true},
		{name: "expired child", age: 31 * 24 * time.Hour, parent: "parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
				for _, dir := range []string{"sessions", "archived_sessions"} {
					home := t.TempDir()
					path := writeRetentionRollout(t, filepath.Join(home, profile, dir), "thread", tc.parent, now.Add(-tc.age))
					if tc.parent != "" {
						writeRetentionRollout(t, filepath.Join(home, profile, dir), tc.parent, "", now.Add(-31*24*time.Hour))
					}
					host := writeRetentionRollout(t, filepath.Join(home, "sessions"), "host", "", now.Add(-tc.age))
					if err := pruneCodexTranscripts(t.Context(), home, now, protectedThreads{ids: tc.protected}); err != nil {
						t.Fatal(err)
					}
					_, err := os.Stat(path)
					if (err == nil) != tc.keep {
						t.Fatalf("retained = %v, want %v (%v)", err == nil, tc.keep, err)
					}
					if _, err := os.Stat(host); err != nil {
						t.Fatal("host rollout removed", err)
					}
				}
			}
		})
	}
}

func writeRetentionRollout(t *testing.T, dir, id, parent string, at time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var source any = "cli"
	if parent != "" {
		source = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]string{"parent_thread_id": parent}}}
	}
	data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "source": source}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+id+".jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneCodexTranscriptsTreesAndLinks(t *testing.T) {
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	for _, mode := range []string{"recent parent", "missing parent", "nested child", "sessions symlink", "profile symlink", "nested symlink", "store failure", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, workerCodexProfileDir, "sessions")
			path := writeRetentionRollout(t, dir, "child", "parent", old)
			db := protectedThreads{}
			switch mode {
			case "recent parent":
				writeRetentionRollout(t, dir, "parent", "", now)
			case "nested child":
				writeRetentionRollout(t, dir, "parent", "grandparent", old)
				db.ids = []string{"grandparent"}
			case "sessions symlink", "profile symlink", "nested symlink":
				target := t.TempDir()
				path = writeRetentionRollout(t, target, "host", "", old)
				link := dir
				if mode == "profile symlink" {
					link = filepath.Dir(dir)
				}
				if mode == "nested symlink" {
					link = filepath.Join(dir, "linked")
				} else if err := os.RemoveAll(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			case "store failure":
				db.err = errors.New("database unavailable")
			case "malformed":
				if err := os.WriteFile(path, []byte("unknown"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			err := pruneCodexTranscripts(t.Context(), home, now, db)
			if !errors.Is(err, db.err) {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("retained file removed", err)
			}
		})
	}
}
