package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A persisted Detent thread may still live in the host's pre-isolation history.
// Copy it on demand so Codex resumes and appends only inside its own profile.
func prepareLegacyCodexRollout(ctx context.Context, profile, threadID string) error {
	launchdCodexProfileMu.Lock()
	defer launchdCodexProfileMu.Unlock()

	local, err := os.OpenRoot(profile)
	if err != nil {
		return err
	}
	defer local.Close()
	path, err := findCodexRollout(ctx, local, threadID, "sessions")
	if err != nil || path != "" {
		return err
	}
	sourceRoot := local
	path, err = findCodexRollout(ctx, local, threadID, "archived_sessions")
	if err != nil {
		return err
	}
	if path == "" {
		host, err := os.OpenRoot(filepath.Dir(profile))
		if err != nil {
			return err
		}
		defer host.Close()
		sourceRoot = host
		path, err = findCodexRollout(ctx, host, threadID, "sessions", "archived_sessions")
		if err != nil {
			return err
		}
	}
	if err != nil || path == "" {
		return err // Let Codex report a genuinely missing thread as before.
	}
	source, err := sourceRoot.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	// Archived legacy threads become active profile threads when resumed.
	relative, err := filepath.Rel(strings.Split(path, string(filepath.Separator))[0], path)
	if err != nil {
		return err
	}
	destination := filepath.Join("sessions", relative)
	if err := local.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	// Publish a complete independent copy atomically without overwriting a
	// rollout another process may already have prepared or resumed.
	temporary := destination + ".detent-" + rand.Text()
	file, err := local.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer local.Remove(temporary)
	_, copyErr := io.Copy(file, source)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return fmt.Errorf("copy legacy Codex rollout: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := local.Chtimes(temporary, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err := local.Link(temporary, destination); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func findCodexRollout(ctx context.Context, root *os.Root, threadID string, directories ...string) (string, error) {
	var found string
	var newest time.Time
	for _, directory := range directories {
		err := fs.WalkDir(root.FS(), directory, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), "-"+threadID+".jsonl") {
				return nil
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
					ID string `json:"id"`
				} `json:"payload"`
			}
			valid := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &meta) == nil && meta.Type == "session_meta" && meta.Payload.ID == threadID
			if err := errors.Join(scanner.Err(), file.Close()); err != nil {
				return err
			}
			if !valid {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if found == "" || info.ModTime().After(newest) {
				found, newest = filepath.FromSlash(path), info.ModTime()
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return found, nil
}
