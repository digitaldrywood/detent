package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

const codexRolloutRetention = 30 * 24 * time.Hour
const codexLogSizeLimit int64 = 1 << 30

var codexLogName = regexp.MustCompile(`^logs_[0-9]+\.sqlite$`)

type codexRetentionStore interface {
	ProtectedCodexSessions(context.Context, time.Time) (map[string]bool, error)
}

func codexRetentionHomes(cfg workflowconfig.Config, lookup func(string) (string, bool)) ([]string, error) {
	var homes []string
	seen := map[string]bool{}
	for _, backend := range cfg.AgentBackendConfigs() {
		if backend.Kind != workflowconfig.AgentBackendCodex {
			continue
		}
		credential, err := codexCredentialPath(backend.Command, lookup, os.UserHomeDir)
		if err != nil {
			return nil, err
		}
		home := filepath.Dir(credential)
		if !seen[home] {
			homes = append(homes, home)
			seen[home] = true
		}
	}
	return homes, nil
}

// Conservatively skip log maintenance when any Codex app-server is running,
// including one owned by another Detent instance. An unavailable process scan
// also prevents maintenance. This never signals a process.
func codexAppServerAlive(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "comm=", "-o", "args=").Output()
	if err != nil {
		return false, err
	}
	return codexAppServerInProcessList(string(out)), nil
}

func codexAppServerInProcessList(output string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if !strings.Contains(strings.ToLower(line), "codex") {
			continue
		}
		for _, field := range fields {
			if field == "app-server" {
				return true
			}
		}
	}
	return false
}

func pruneCodexLogs(ctx context.Context, home string, now time.Time, limit int64, alive func(context.Context) (bool, error)) error {
	running, err := alive(ctx)
	if err != nil || running {
		return err
	}
	for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
		dir := filepath.Join(home, profile)
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !codexLogName.MatchString(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if userInfo, err := os.Stat(filepath.Join(home, entry.Name())); err == nil && os.SameFile(info, userInfo) {
				continue
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			// Reject shared sidecars as well as shared main databases.
			safe := true
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if side, err := os.Lstat(path + suffix); err == nil && !side.Mode().IsRegular() {
					safe = false
				} else if err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				side, sideErr := os.Stat(path + suffix)
				userSide, userErr := os.Stat(filepath.Join(home, entry.Name()+suffix))
				if sideErr != nil && !errors.Is(sideErr, os.ErrNotExist) {
					return sideErr
				}
				if userErr != nil && !errors.Is(userErr, os.ErrNotExist) {
					return userErr
				}
				if sideErr == nil && userErr == nil && os.SameFile(side, userSide) {
					safe = false
				}
			}
			if !safe {
				continue
			}
			if err := pruneCodexLog(ctx, path, now, limit, alive); err != nil {
				return fmt.Errorf("prune %s: %w", path, err)
			}
		}
	}
	return nil
}

func pruneCodexLog(ctx context.Context, path string, now time.Time, limit int64, alive func(context.Context) (bool, error)) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	_, pruneErr := db.ExecContext(ctx, "DELETE FROM logs WHERE ts < ?", now.Add(-7*24*time.Hour).Unix())
	if pruneErr == nil {
		_, pruneErr = db.ExecContext(ctx, "VACUUM")
	}
	if pruneErr == nil {
		_, pruneErr = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	if err := errors.Join(pruneErr, db.Close()); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() <= limit {
		return nil
	}
	// VACUUM can take time on a large log. Recheck before unlinking a database
	// that another instance may have opened during maintenance.
	running, err := alive(ctx)
	if err != nil || running {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

type codexRollout struct {
	path, id string
	size     int64
	modified time.Time
	started  time.Time
}

func detentRollouts(ctx context.Context, home string) ([]codexRollout, error) {
	root := filepath.Join(home, "sessions")
	resolved, err := filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rollouts []codexRollout
	err = filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				ID         string `json:"id"`
				Originator string `json:"originator"`
				Timestamp  string `json:"timestamp"`
			} `json:"payload"`
		}
		valid := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &meta) == nil
		scanErr := scanner.Err()
		closeErr := f.Close()
		if scanErr != nil {
			return nil
		}
		if closeErr != nil {
			return closeErr
		}
		if !valid || meta.Type != "session_meta" || meta.Payload.Originator != "detent-orchestrator" || meta.Payload.ID == "" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var started time.Time
		if meta.Payload.Timestamp != "" {
			started, err = time.Parse(time.RFC3339Nano, meta.Payload.Timestamp)
			if err != nil {
				return nil
			} // Uncertain session age must not authorize deletion.
		}
		rollouts = append(rollouts, codexRollout{path: path, id: meta.Payload.ID, size: info.Size(), modified: info.ModTime(), started: started})
		return nil
	})
	return rollouts, err
}

func pruneCodexRollouts(ctx context.Context, home string, now time.Time, source codexRetentionStore) (int64, error) {
	rollouts, err := detentRollouts(ctx, home)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-codexRolloutRetention)
	protected, err := source.ProtectedCodexSessions(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	for _, rollout := range rollouts {
		if !rollout.modified.Before(cutoff) || !rollout.started.Before(cutoff) {
			protected[rollout.id] = true
		}
	}
	var removed int64
	for _, rollout := range rollouts {
		if protected[rollout.id] || !rollout.modified.Before(cutoff) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		info, err := os.Lstat(rollout.path)
		if err != nil {
			return removed, err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Equal(rollout.modified) || info.Size() != rollout.size {
			continue
		}
		if err := os.Remove(rollout.path); err != nil {
			return removed, err
		}
		removed += rollout.size
	}
	return removed, nil
}

func codexRolloutSweep(source any, logger *slog.Logger) func(context.Context, workflowconfig.Config) error {
	retentionStore, ok := source.(codexRetentionStore)
	if !ok {
		return nil
	}
	return func(ctx context.Context, cfg workflowconfig.Config) error {
		homes, err := codexRetentionHomes(cfg, os.LookupEnv)
		if err != nil {
			return err
		}
		for _, home := range homes {
			removed, err := pruneCodexRollouts(ctx, home, time.Now(), retentionStore)
			if err != nil {
				return err
			}
			if removed > 0 {
				logger.Info("pruned Detent Codex rollouts", "home", home, "removed_bytes", removed)
			}
		}
		return nil
	}
}

func checkDoctorCodexStorage(ctx context.Context, id string, cfg workflowconfig.Config, lookup func(string) string) []doctorCheck {
	env := os.LookupEnv
	if lookup != nil {
		env = func(key string) (string, bool) { value := lookup(key); return value, value != "" }
	}
	homes, err := codexRetentionHomes(cfg, env)
	if err != nil {
		return []doctorCheck{{Name: "Project " + id + " Codex storage", Status: doctorWarn, Detail: err.Error()}}
	}
	var checks []doctorCheck
	for _, home := range homes {
		check := doctorCheck{Name: "Project " + id + " Codex storage " + home, Status: doctorOK}
		var total int64
		var details []string
		for _, profile := range []string{workerCodexProfileDir, launchdCodexProfileDir} {
			entries, err := os.ReadDir(filepath.Join(home, profile))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				check.Status = doctorWarn
				details = append(details, err.Error())
				continue
			}
			for _, entry := range entries {
				if !codexSQLiteFilename(entry.Name()) {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					check.Status = doctorWarn
					details = append(details, err.Error())
					continue
				}
				if !info.Mode().IsRegular() {
					continue
				}
				total += info.Size()
				details = append(details, fmt.Sprintf("%s/%s: %d bytes", profile, entry.Name(), info.Size()))
			}
		}
		rollouts, err := detentRollouts(ctx, home)
		if err != nil {
			check.Status = doctorWarn
			details = append(details, err.Error())
		}
		var rolloutBytes int64
		for _, rollout := range rollouts {
			rolloutBytes += rollout.size
		}
		total += rolloutBytes
		details = append(details, fmt.Sprintf("Detent rollouts: %d bytes; combined: %d bytes", rolloutBytes, total))
		check.Detail = strings.Join(details, "; ")
		if total > 2<<30 {
			check.Status = doctorWarn
			check.Hint = "Combined Codex storage exceeds 2 GiB. Boot retains seven days of worker logs; the existing reaper retains thirty days of unreferenced Detent rollouts."
		}
		checks = append(checks, check)
	}
	return checks
}
