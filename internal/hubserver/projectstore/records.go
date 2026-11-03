package projectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrActiveWork = errors.New("project has active work")
var ErrStateInUse = errors.New("workflow states are used by work items")
var ErrInvalidOnboardingProgress = errors.New("project onboarding progress is invalid")

type Settings struct {
	OrganizationID            tracker.OrganizationID
	ProjectID                 tracker.ProjectID
	Revision                  int64
	Intake                    string
	Projection                string
	RepositoryEnabled         bool
	Repository                string
	CheckoutRepository        string
	AgingEnabled              bool
	DuplicateChallenge        bool
	ArchiveCompletedAfterDays *int64
	ArchiveCancelledAfterDays *int64
	WorkflowKey               string
	WorkflowSHA256            string
	WorkflowBytes             int64
	WorkflowContentBytes      int64
	RequireVisualAttachments  bool
	ModelRevision             int64
	ModelKey                  string
	ModelSHA256               string
	ModelBytes                int64
	ModelContentBytes         int64
}

type Record struct {
	Core     corestore.Project
	Settings Settings
	States   []tracker.NativeState
}

type Reader interface {
	ProjectLatestAttemptStatus(context.Context, tracker.ProjectID) (string, error)
	ProjectOnboardingProgress(context.Context, tracker.ProjectID) (onboarding.Progress, error)
	ProjectAllowanceUsage(context.Context, int64) (int64, error)
	ProjectRecord(context.Context, tracker.ProjectID) (Record, error)
	ProjectBySlug(context.Context, string) (Record, error)
	ProjectSlugOwner(context.Context, string) (tracker.ProjectID, error)
}

type GitHubRepositoryBinding struct {
	ProjectID          tracker.ProjectID
	Repository         string
	CheckoutRepository string
}

type GitHubRepositoryBindingsPageReader interface {
	GitHubRepositoryBindings(context.Context, tracker.ProjectID, int) ([]GitHubRepositoryBinding, error)
}

type Writer interface {
	Reader
	SaveProjectOnboardingProgress(context.Context, tracker.ProjectID, onboarding.Progress, tracker.Revision) error
	InitializeProjectAccess(context.Context, tracker.ProjectID, string) error
	CreateProjectRecord(context.Context, Record) (Record, error)
	UpdateProjectRecord(context.Context, Record) (Record, error)
	RetireProject(context.Context, tracker.ProjectID, int64, time.Time) error
	UpdateProjectContent(context.Context, tracker.ProjectID, int64, bool) error
	UpdateProjectModel(context.Context, Settings) error
	RequireProjectIdle(context.Context, tracker.ProjectID, time.Time) error
	UpdateProjectSlug(context.Context, tracker.ProjectID, string, string) error
}

func Slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	value := strings.Trim(b.String(), "-")
	if len(value) > 60 {
		value = strings.TrimRight(value[:60], "-")
	}
	if value == "" {
		value = "project"
	}
	if readableurl.Validate(value) != nil {
		value += "-project"
	}
	return value
}

func CollisionSlug(base string, id tracker.ProjectID) string {
	digest := sha256.Sum256([]byte(id))
	return base + "-" + hex.EncodeToString(digest[:6])
}
