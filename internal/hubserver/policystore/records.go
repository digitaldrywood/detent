package policystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Revision struct {
	OrganizationID      tracker.OrganizationID
	ProjectID           tracker.ProjectID
	PolicyID            string
	GitHubPullRequest   *bool
	ApprovedBy          string
	ApprovedAt          time.Time
	PayloadObjectKey    string
	PayloadSHA256       string
	PayloadBytes        int64
	PayloadContentBytes int64
}

type LeasePolicy struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	LeaseID        tracker.LeaseID
	PolicyID       string
	BoundAt        time.Time
}

type Reader interface {
	ModelReader
	LeasePolicy(context.Context, tracker.LeaseID) (LeasePolicy, error)
	ProjectPolicy(context.Context, tracker.ProjectID) (Revision, error)
	ProjectPolicyRevision(context.Context, tracker.ProjectID, string) (Revision, error)
}

type Writer interface {
	ModelWriter
	Reader
	BindLeasePolicy(context.Context, LeasePolicy) error
	ApproveProjectPolicy(context.Context, Revision, string) error
}
