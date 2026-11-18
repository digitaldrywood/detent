package store

import (
	"context"
	"time"
)

type SessionProgressStore interface {
	SessionProgress(context.Context, int64) (SessionProgress, error)
	SaveSessionProgress(context.Context, int64, SessionProgress) error
}

type SessionProgress struct {
	HeadSHA              string    `json:"head_sha,omitempty"`
	CommitFingerprint    string    `json:"commit_fingerprint,omitempty"`
	TrackedFingerprint   string    `json:"tracked_fingerprint,omitempty"`
	WorkspaceFingerprint string    `json:"workspace_fingerprint,omitempty"`
	WorkpadFingerprint   string    `json:"workpad_fingerprint,omitempty"`
	Local                bool      `json:"local,omitempty"`
	LastProgressAt       time.Time `json:"last_progress_at"`
}
