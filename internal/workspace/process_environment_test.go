package workspace

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestWorkerScratchEnvironmentMatches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, tt := range []struct {
		name string
		env  []string
		want bool
	}{
		{name: "unmarked"},
		{name: "legacy scratch", env: []string{"TMPDIR=" + filepath.Join(root, ".detent", "tmp")}, want: true},
		{name: "windows temp", env: []string{"Temp=" + root}, want: runtime.GOOS == "windows"},
		{name: "neighbor prefix", env: []string{"TMPDIR=" + root + "-neighbor"}},
		{name: "relative path", env: []string{"TMPDIR=relative"}},
		{name: "unrelated reference", env: []string{"PATH=" + root}},
		{name: "changed temp retains owner", env: []string{"TMPDIR=" + t.TempDir(), "DETENT_WORKER_SCRATCH=" + root}, want: true},
		{name: "explicit other owner", env: []string{"TMPDIR=" + root, "DETENT_WORKER_SCRATCH=" + t.TempDir()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerScratchEnvironmentMatches(root, tt.env); got != tt.want {
				t.Fatalf("ownership match = %t, want %t", got, tt.want)
			}
		})
	}
}
