package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type transcriptRetentionStore interface {
	ProtectedCodexThreadIDs(context.Context) ([]string, error)
}

type retainedRollout struct {
	path, id, parent string
	old              bool
}

// pruneCodexTranscripts never opens the host sessions directory. OpenRoot bounds
// all file access to a real profile, including removal if a path changes mid-walk.
func pruneCodexTranscripts(ctx context.Context, home string, now time.Time, db transcriptRetentionStore) error {
	ids, err := db.ProtectedCodexThreadIDs(ctx)
	if err != nil {
		return err
	}
	for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
		path := filepath.Join(home, profile)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		root, err := os.OpenRoot(path)
		if err != nil {
			return err
		}
		err = pruneProfileTranscripts(ctx, root, now.Add(-30*24*time.Hour), ids)
		if err = errors.Join(err, root.Close()); err != nil {
			return err
		}
	}
	return nil
}

func pruneProfileTranscripts(ctx context.Context, root *os.Root, cutoff time.Time, ids []string) error {
	keep := make(map[string]bool)
	for _, id := range ids {
		keep[id] = true
	}
	var files []retainedRollout
	for _, directory := range []string{"sessions", "archived_sessions"} {
		info, err := root.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		} // Old shared symlinks must never be traversed.
		err = fs.WalkDir(root.FS(), directory, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			file, err := root.Open(path)
			if err != nil {
				return err
			}
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 4096), 1024*1024)
			var meta struct {
				Type    string `json:"type"`
				Payload struct {
					ID     string          `json:"id"`
					Source json.RawMessage `json:"source"`
				} `json:"payload"`
			}
			valid := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &meta) == nil && meta.Type == "session_meta" && meta.Payload.ID != ""
			if err := file.Close(); err != nil {
				return err
			}
			// Unknown metadata is retained, never guessed from a filename.
			if !valid {
				return nil
			}
			var source struct {
				Subagent struct {
					ThreadSpawn struct {
						ParentThreadID string `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			}
			// Interactive sources are strings; subagent sources are objects.
			if len(meta.Payload.Source) > 0 && meta.Payload.Source[0] == '{' {
				if json.Unmarshal(meta.Payload.Source, &source) != nil {
					keep[meta.Payload.ID] = true
					return nil
				}
			}
			old := info.ModTime().Before(cutoff)
			files = append(files, retainedRollout{path: path, id: meta.Payload.ID, parent: source.Subagent.ThreadSpawn.ParentThreadID, old: old})
			if !old {
				keep[meta.Payload.ID] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	known := make(map[string]bool)
	for _, file := range files {
		known[file.id] = true
	}
	for _, file := range files {
		// A missing or unreadable parent may live in retained legacy history.
		if file.parent != "" && !known[file.parent] {
			keep[file.parent] = true
		}
	}
	// Propagate through arbitrary-depth child trees, independent of walk order.
	for changed := true; changed; {
		changed = false
		for _, file := range files {
			if keep[file.parent] && !keep[file.id] {
				keep[file.id] = true
				changed = true
			}
		}
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.old && !keep[file.id] {
			if err := root.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
