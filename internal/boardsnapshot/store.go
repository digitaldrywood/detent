package boardsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

const SchemaVersion = 1

var ErrInvalidConfig = errors.New("board snapshot store configuration is invalid")

type Store interface {
	Load(context.Context) (telemetry.Snapshot, bool, error)
	Save(context.Context, telemetry.Snapshot) error
}

type Config struct {
	Path   string
	MaxAge time.Duration
	Now    func() time.Time
}

type fileStore struct {
	path   string
	maxAge time.Duration
	now    func() time.Time
}

type envelope struct {
	Schema   int                `json:"schema"`
	SavedAt  time.Time          `json:"saved_at"`
	Snapshot telemetry.Snapshot `json:"snapshot"`
}

func New(cfg Config) (Store, error) {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, fmt.Errorf("%w: path is required", ErrInvalidConfig)
	}
	if cfg.MaxAge <= 0 {
		return nil, fmt.Errorf("%w: max age must be positive", ErrInvalidConfig)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &fileStore{path: path, maxAge: cfg.MaxAge, now: now}, nil
}

func (s *fileStore) Load(ctx context.Context) (telemetry.Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return telemetry.Snapshot{}, false, err
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return telemetry.Snapshot{}, false, nil
	}
	if err != nil {
		return telemetry.Snapshot{}, false, fmt.Errorf("read board snapshot: %w", err)
	}
	var cached envelope
	if err := json.Unmarshal(raw, &cached); err != nil {
		return telemetry.Snapshot{}, false, fmt.Errorf("decode board snapshot: %w", err)
	}
	if cached.Schema != SchemaVersion {
		return telemetry.Snapshot{}, false, fmt.Errorf("decode board snapshot: unsupported schema %d", cached.Schema)
	}
	if cached.SavedAt.IsZero() || cached.Snapshot.GeneratedAt.IsZero() {
		return telemetry.Snapshot{}, false, errors.New("decode board snapshot: timestamps are required")
	}
	if s.now().Sub(cached.SavedAt) > s.maxAge {
		return telemetry.Snapshot{}, false, nil
	}
	cached.Snapshot.LastKnown = true
	cached.Snapshot.LastKnownUntil = cached.SavedAt.Add(s.maxAge)
	return cached.Snapshot, true, nil
}

func (s *fileStore) Save(ctx context.Context, snapshot telemetry.Snapshot) (saveErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot.GeneratedAt.IsZero() {
		return errors.New("save board snapshot: generated timestamp is required")
	}
	snapshot.LastKnown = false
	raw, err := json.Marshal(envelope{
		Schema:   SchemaVersion,
		SavedAt:  s.now().UTC(),
		Snapshot: snapshot,
	})
	if err != nil {
		return fmt.Errorf("encode board snapshot: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create board snapshot directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".board-snapshot-*")
	if err != nil {
		return fmt.Errorf("create temporary board snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath == "" {
			return
		}
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			saveErr = errors.Join(saveErr, fmt.Errorf("remove temporary board snapshot: %w", err))
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return closeTemporarySnapshot(temporary, fmt.Errorf("secure temporary board snapshot: %w", err))
	}
	if _, err := temporary.Write(raw); err != nil {
		return closeTemporarySnapshot(temporary, fmt.Errorf("write temporary board snapshot: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		return closeTemporarySnapshot(temporary, fmt.Errorf("sync temporary board snapshot: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary board snapshot: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return fmt.Errorf("replace board snapshot: %w", err)
	}
	temporaryPath = ""
	return nil
}

func closeTemporarySnapshot(file *os.File, operationErr error) error {
	if err := file.Close(); err != nil {
		return errors.Join(operationErr, fmt.Errorf("close temporary board snapshot: %w", err))
	}
	return operationErr
}

func Eligible(snapshot telemetry.Snapshot) bool {
	if snapshot.LastKnown || snapshot.GeneratedAt.IsZero() {
		return false
	}
	if !hasRefreshSignal(snapshot.Refresh) {
		return true
	}
	status := snapshot.Refresh.ReadinessStatus()
	if status == telemetry.RefreshStatusReady || status == telemetry.RefreshStatusBehind {
		return true
	}
	return status == telemetry.RefreshStatusDegraded && carriesTrackerData(snapshot)
}

func hasRefreshSignal(refresh telemetry.Refresh) bool {
	return refresh.PollIntervalSeconds != 0 ||
		refresh.Status != "" ||
		refresh.LastRefreshAt != nil ||
		refresh.NextRefreshAt != nil ||
		strings.TrimSpace(refresh.LastError) != "" ||
		refresh.LastErrorAt != nil
}

func carriesTrackerData(snapshot telemetry.Snapshot) bool {
	if snapshot.Refresh.LastRefreshAt != nil ||
		len(snapshot.BoardIssues) > 0 ||
		len(snapshot.Pipeline) > 0 ||
		len(snapshot.Running) > 0 ||
		len(snapshot.Queue) > 0 ||
		len(snapshot.Blocked) > 0 ||
		len(snapshot.Completed) > 0 ||
		snapshot.Counts != (telemetry.Counts{}) {
		return true
	}
	for _, project := range snapshot.Projects {
		if project.Refresh.LastRefreshAt != nil ||
			project.Refresh.Ready() ||
			project.Counts != (telemetry.Counts{}) {
			return true
		}
	}
	return false
}
