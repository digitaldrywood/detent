package hubrecords

import (
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeClaimPlan struct {
	SnapshotLoaded       bool                            `json:"-"`
	Ranking              *runnerstore.ProjectRanking     `json:"-"`
	ReleaseDelivery      ReleaseDeliveryContractFacts    `json:"-"`
	Observations         *tenantstore.ChangeVersionRows  `json:"-"`
	Approval             *ChangeVersionApproval          `json:"-"`
	Candidate            claimstore.Candidate            `json:"-"`
	Project              projectstore.Record             `json:"-"`
	Policy               policystore.Revision            `json:"-"`
	Descriptor           policy.Descriptor               `json:"-"`
	Item                 corestore.WorkItem              `json:"-"`
	Issue                tracker.NativeIssue             `json:"-"`
	Version              eventstore.CollaborationVersion `json:"-"`
	Linked               *tracker.LinkedIssueSource      `json:"-"`
	LinkedRecord         *eventstore.LinkedSource        `json:"-"`
	Change               tracker.NativeChangeReference   `json:"-"`
	ChangeReady          bool                            `json:"-"`
	ChangeFacts          *tenantstore.BoardChangeRows    `json:"-"`
	ChangeRecord         eventstore.Change               `json:"-"`
	ChangePolicy         CurrentChangePolicyRows         `json:"-"`
	ChangeVersion        eventstore.Version              `json:"-"`
	ChangePolicyRevision policystore.Revision            `json:"-"`
	LatestAttempt        leasestore.Attempt              `json:"-"`
	LatestRunEvent       eventstore.Event                `json:"-"`
	LatestAttemptFound   bool                            `json:"-"`
	LatestRunEventFound  bool                            `json:"-"`
	LandingPending       bool                            `json:"-"`
	LandingRequired      bool                            `json:"-"`
	Replay               *leasestore.Lease               `json:"-"`
	Placement            *ClaimPlacementSnapshot         `json:"-"`
	Source               ClaimSourceFacts                `json:"-"`
}

type ClaimSourceFacts struct {
	Loaded                              bool
	AttemptID, CheckpointObjectKey      string
	Checkpoint                          *tracker.NativeCheckpoint
	CheckpointMachine, CheckpointRunner string
	ChangeID, VersionID                 string
	VersionMachine, VersionRunner       string
	VersionStored, CheckpointUnreadable bool
}

func SameNativeClaimProjectPolicy(left, right policystore.Revision) bool {
	return SameNativeClaimPolicyRevision(left, right) && left.ApprovedBy == right.ApprovedBy && left.ApprovedAt.Equal(right.ApprovedAt)
}

func SameNativeClaimPolicyRevision(left, right policystore.Revision) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && SameNativeClaimOptionalBool(left.GitHubPullRequest, right.GitHubPullRequest) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func SameNativeClaimOptionalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

type PlacementDecision struct {
	Policy        policy.Placement `json:"policy"`
	ReadyTodo     int              `json:"ready_todo"`
	LocalFree     int              `json:"local_free"`
	SpriteFree    int              `json:"sprite_free"`
	SpriteUsed    int              `json:"sprite_used"`
	Pending       int              `json:"pending"`
	SpriteTarget  int              `json:"sprite_target"`
	Provisionable int              `json:"provisionable"`
	ProviderFree  *int             `json:"provider_free,omitempty"`
	Reason        string           `json:"reason"`
}

type PlacementRunner struct {
	runnerauth.Runner
	Sprite, Pending, Paused bool
	Enrollment              string
}

type PlacementSnapshot struct {
	PlacementDecision
	Runners      []PlacementRunner
	Requirements policy.Requirements
	Items        map[tracker.WorkItemID]providercapacity.Requirement
	Local        map[tracker.WorkItemID]int
	Resolver     *runner.BoardIdentityResolver `json:"-"`
}

type ClaimPlacementSnapshot struct {
	Snapshot *PlacementSnapshot `json:"-"`
}
