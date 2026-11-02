package policystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
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

type Observation struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	RunnerID       string
	PolicyID       string
	ObservedAt     time.Time
	Source         *policy.RepositorySource
	Revision       Revision
}

type ObservedRevision struct {
	Observation
	PreviouslyApproved  bool
	DistinctPolicyCount int64
	RunnerCount         int64
	RepositoryApproved  bool
}

type WorkflowApply struct {
	OrganizationID           tracker.OrganizationID
	ProjectID                tracker.ProjectID
	ID                       int64
	PolicyID                 string
	PreviousPolicyID         string
	Repository               string
	SourceCommit             string
	PreviousDefinitionDigest string
	DefinitionDigest         string
	RunnerID                 string
	AppliedBy                string
	AppliedAt                time.Time
	ChangeID                 string
	PullRequestURL           string
}

type Reader interface {
	ModelReader
	LeasePolicy(context.Context, tracker.LeaseID) (LeasePolicy, error)
	ProjectPolicy(context.Context, tracker.ProjectID) (Revision, error)
	ProjectPolicyRevision(context.Context, tracker.ProjectID, string) (Revision, error)
	ProjectPolicyObservations(context.Context, tracker.ProjectID, string, string, time.Time, int) ([]ObservedRevision, int64, error)
	CurrentProjectPolicyObservation(context.Context, tracker.ProjectID, string, time.Time) (bool, error)
	ProjectWorkflowApplies(context.Context, tracker.ProjectID, int64, int) ([]WorkflowApply, error)
}

type Writer interface {
	ModelWriter
	Reader
	BindLeasePolicy(context.Context, LeasePolicy) error
	ApproveProjectPolicy(context.Context, Revision, string) error
	ImportProjectPolicyRevision(context.Context, Revision, bool) error
	ImportProjectPolicyCurrent(context.Context, tracker.ProjectID, string) error
	RecordProjectPolicyObservation(context.Context, Observation) (bool, error)
	CreateProjectWorkflowApply(context.Context, WorkflowApply) (WorkflowApply, error)
}
