package localboard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Comment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Record struct {
	Key          string    `json:"key"`
	Kind         string    `json:"kind"`
	SourceID     string    `json:"source_id"`
	Number       int       `json:"number"`
	Repository   string    `json:"repository,omitempty"`
	Title        string    `json:"title"`
	Priority     *int      `json:"priority,omitempty"`
	Body         string    `json:"body"`
	State        string    `json:"state"`
	Author       string    `json:"author"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Comments     []Comment `json:"comments"`
	Dependencies []string  `json:"dependencies"`
}

type Request struct {
	Record Record `json:"record"`
	DryRun bool   `json:"dry_run"`
}

type Result struct {
	SourceState  string                   `json:"source_state"`
	SourceID     string                   `json:"source_id"`
	WorkItemID   tracker.NativeWorkItemID `json:"work_item_id,omitempty"`
	Action       string                   `json:"action"`
	State        string                   `json:"state"`
	UnknownLane  bool                     `json:"unknown_lane,omitempty"`
	Comments     int                      `json:"comments"`
	Dependencies int                      `json:"dependencies"`
}

func SourceKey(store, project, kind, id string) string {
	raw := []byte{}
	for _, value := range []string{store, project, kind, id} {
		raw = strconv.AppendQuote(raw, value)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func Validate(record Record) error {
	if len(record.Key) != 64 || record.Kind != "local_sqlite" && record.Kind != "github_local" || record.SourceID == "" || len(record.SourceID) > 500 || len(record.Author) > 200 || strings.TrimSpace(record.Author) == "" {
		return errors.New("invalid local board source identity")
	}
	if _, err := hex.DecodeString(record.Key); err != nil {
		return errors.New("invalid local board source key")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) || record.UpdatedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("invalid local board source timestamps")
	}
	if len(record.Comments) > 1000 || len(record.Dependencies) > 100 {
		return errors.New("local board item exceeds comment or dependency bound")
	}
	seen := map[string]bool{}
	for _, comment := range record.Comments {
		if comment.ID == "" || len(comment.ID) > 200 || seen[comment.ID] || strings.TrimSpace(comment.Body) == "" || len(comment.Body) > 64<<10 || comment.CreatedAt.IsZero() || comment.UpdatedAt.Before(comment.CreatedAt) || comment.UpdatedAt.After(time.Now().Add(time.Minute)) {
			return errors.New("invalid local board comment")
		}
		seen[comment.ID] = true
	}
	for _, key := range record.Dependencies {
		if len(key) != 64 {
			return errors.New("invalid local board dependency key")
		}
	}
	if record.Kind == "github_local" && (record.Number <= 0 || len(strings.Split(record.Repository, "/")) != 2) {
		return errors.New("invalid local board GitHub identity")
	}
	return nil
}
