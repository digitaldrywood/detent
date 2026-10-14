package project_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
)

func TestNewRemovesLegacyCache(t *testing.T) {
	for _, tilde := range []bool{false, true} {
		t.Run(map[bool]string{false: "absolute", true: "tilde"}[tilde], func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, ".detent", "cache")
			if err := os.MkdirAll(cache, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cache, "entry"), []byte("123"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerMemory
			cfg.Workspace.Root = root
			if tilde {
				t.Setenv("HOME", filepath.Dir(root))
				t.Setenv("USERPROFILE", filepath.Dir(root))
				cfg.Workspace.Root = "~/" + filepath.Base(root)
			}
			var logs bytes.Buffer
			_, err := project.New(project.Config{
				Project:  globalconfig.Project{ID: "cache-test", Workdir: t.TempDir(), Weight: 1},
				Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."},
			}, project.Dependencies{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("legacy cache remains: %v", err)
			}
			for _, want := range []string{"removed legacy worker caches", "bytes_reclaimed=3", "workspace_root=" + root} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log %q missing %q", logs.String(), want)
				}
			}
		})
	}
}
