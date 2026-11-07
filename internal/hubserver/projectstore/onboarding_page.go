package projectstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OnboardingViewer struct {
	ID, Hash, SessionHash, Scope, UserID, Membership, Principal, Role  string
	KeyScope, KeyKind, ProjectAccess, RunnerID, EntryKind, EntryAccess string
	EntryID, EntryBinding, EntryPermission                             string
	NativeOnly                                                         bool
	Projects, BlockedProjects                                          []string
}

type OnboardingCandidate struct {
	ProjectID        tracker.ProjectID
	ViewerKey        string
	Viewer           OnboardingViewer
	Generation       int64
	AccessGeneration int64
	SourceAt         time.Time
}

type OnboardingPageReader interface {
	OnboardingPage(context.Context, tracker.ProjectID, string) (onboarding.Project, error)
}

type OnboardingPageWriter interface {
	RequestOnboardingPage(context.Context, tracker.ProjectID, string, OnboardingViewer) error
	ClaimOnboardingPage(context.Context) (OnboardingCandidate, error)
	PublishOnboardingPage(context.Context, OnboardingCandidate, onboarding.Project, time.Time) error
	DiscardOnboardingPage(context.Context, OnboardingCandidate) error
}
