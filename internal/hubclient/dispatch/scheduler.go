package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubclient/claim"
	isolationpolicy "github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SchedulerConfig struct {
	HubProcessReceipts bool
	ExecutionFactory   ExecutionFactory
	CompletionRestorer CompletionRestorer
	ReleasedStopped    func(context.Context, tracker.NativeWorkItemID, tracker.NativeAttempt) (bool, error)
	AvailableCapacity  func(context.Context, int) int

	StorageConstraint func(context.Context) string

	RefreshProjects       func(context.Context, *Scheduler) error
	Skills                func(project string) []skills.ProviderSkill
	PrepareProject        func(context.Context, string) error
	RunnerSetupDeclared   func(context.Context, string) *bool
	UpdateOwner           func(context.Context, *runnerauth.UpdateRequest) *runnerauth.UpdateObservation
	CapacityConfiguration func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig
	// LocalChecks are startup observations by local project, reused by the heartbeat owner.
	LocalChecks        map[string]runnerauth.LocalChecks
	GitHubDiscovery    func(context.Context, tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error)
	GitHubIntake       func(context.Context, string) (tracker.GitHubIssueSnapshot, error)
	Problems           func() []runnerauth.Problem
	IsolationReport    func(context.Context) (isolationpolicy.Report, []runnerauth.Problem)
	ProviderReports    func() ([]providercapacity.Report, error)
	OrganizationID     tracker.OrganizationID
	NativeProjects     map[string]tracker.ProjectID
	CheckoutRepository func(project string) string
	Machine            Machine
	HeartbeatInterval  time.Duration
	LeaseTTL           time.Duration
	Now                func() time.Time
	SessionID          func() (string, error)
	// LeaseHold keeps the host awake while a native lease is held; nil selects
	// the Sprite task hold on a Sprite and nothing elsewhere.
	LeaseHold func(context.Context) (func(), error)
}

type Scheduler struct {
	hubProcessReceipts bool
	executionFactory   ExecutionFactory
	completionRestorer CompletionRestorer
	controlMu          sync.RWMutex
	controlPaused      atomic.Bool
	releasedStopped    func(context.Context, tracker.NativeWorkItemID, tracker.NativeAttempt) (bool, error)
	availableCapacity  func(context.Context, int) int

	storageConstraint func(context.Context) string

	hostMetrics           *hostmetrics.Collector
	hostMetricsMu         sync.Mutex
	refreshProjects       func(context.Context, *Scheduler) error
	projectsMu            sync.RWMutex
	skills                func(project string) []skills.ProviderSkill
	projectConfiguration  func(context.Context, string, *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration
	prepareProject        func(context.Context, string) error
	runnerSetupDeclared   func(context.Context, string) *bool
	claimAdmission        func() (func(), bool)
	updateOwner           func(context.Context, *runnerauth.UpdateRequest) *runnerauth.UpdateObservation
	capacityConfiguration func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig
	localChecks           map[string]runnerauth.LocalChecks
	nativeAdmissions      map[string]tracker.NativeAdmissionObservation
	githubDiscovery       func(context.Context, tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error)
	githubIntake          func(context.Context, string) (tracker.GitHubIssueSnapshot, error)
	problems              func() []runnerauth.Problem
	isolationReport       func(context.Context) (isolationpolicy.Report, []runnerauth.Problem)
	lastIsolationAt       time.Time
	lastIsolation         isolationpolicy.Report
	providerReports       func() ([]providercapacity.Report, error)
	claimPolicies         map[string]claimPolicy
	nativeProjects        map[string]*NativeConnector
	nativeClaims          map[string]nativeClaim
	nativeHeartbeats      map[tracker.ProjectID]time.Time
	configHeartbeats      map[tracker.ProjectID]time.Time
	checkoutRepository    func(project string) string
	reportedPolicies      map[string]string
	client                *Client
	machine               Machine
	heartbeatInterval     time.Duration
	leaseTTL              time.Duration
	now                   func() time.Time
	sessionID             func() (string, error)
	mu                    sync.Mutex
	nativeHeartbeatMu     map[tracker.ProjectID]*sync.Mutex
	claims                map[string]tracker.Lease
	leaseHold             func(context.Context) (func(), error)
	leaseHoldRelease      func()
	leaseHoldPending      chan struct{}
	nativeFetches         int
	githubBatchRunning    bool
}

func (s *Scheduler) PauseController() func() {
	s.controlMu.Lock()
	s.controlPaused.Store(true)
	s.controlMu.Unlock()
	return func() {
		s.controlMu.Lock()
		s.controlPaused.Store(false)
		s.controlMu.Unlock()
	}
}

func NewScheduler(client *Client, config SchedulerConfig) (*Scheduler, error) {
	if client == nil {
		return nil, errors.New("hub client is required")
	}
	config.Machine.ID = tracker.MachineID(strings.TrimSpace(string(config.Machine.ID)))
	config.Machine.Hostname = strings.TrimSpace(config.Machine.Hostname)
	if config.Machine.ID == "" || config.Machine.Hostname == "" || config.Machine.Capacity <= 0 || strings.TrimSpace(config.Machine.Version) == "" {
		return nil, errors.New("hub machine ID, hostname, positive capacity, and version are required")
	}
	if config.HeartbeatInterval <= 0 || config.LeaseTTL <= 0 || config.HeartbeatInterval >= config.LeaseTTL {
		return nil, errors.New("hub heartbeat interval must be positive and shorter than the lease TTL")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	sessionID := config.SessionID
	if sessionID == nil {
		sessionID = randomSessionID
	}
	scheduler := &Scheduler{executionFactory: config.ExecutionFactory, completionRestorer: config.CompletionRestorer,
		releasedStopped:    config.ReleasedStopped,
		hubProcessReceipts: config.HubProcessReceipts,
		availableCapacity:  config.AvailableCapacity,

		storageConstraint: config.StorageConstraint,

		refreshProjects:       config.RefreshProjects,
		prepareProject:        config.PrepareProject,
		runnerSetupDeclared:   config.RunnerSetupDeclared,
		skills:                config.Skills,
		capacityConfiguration: config.CapacityConfiguration,
		updateOwner:           config.UpdateOwner,
		localChecks:           config.LocalChecks,
		nativeAdmissions:      make(map[string]tracker.NativeAdmissionObservation),
		githubIntake:          config.GitHubIntake,
		githubDiscovery:       config.GitHubDiscovery,
		problems:              config.Problems,
		isolationReport:       config.IsolationReport,
		providerReports:       config.ProviderReports,
		claimPolicies:         make(map[string]claimPolicy),
		client:                client, machine: config.Machine, heartbeatInterval: config.HeartbeatInterval,
		leaseTTL: config.LeaseTTL, now: now, sessionID: sessionID, claims: make(map[string]tracker.Lease),
		nativeProjects: make(map[string]*NativeConnector), nativeClaims: make(map[string]nativeClaim), nativeHeartbeats: make(map[tracker.ProjectID]time.Time),
		checkoutRepository: config.CheckoutRepository,
		leaseHold:          config.LeaseHold,
	}
	if scheduler.leaseHold == nil {
		scheduler.leaseHold = defaultLeaseHold()
	}
	for project, id := range config.NativeProjects {
		native, err := client.Native(config.OrganizationID, id)
		if err != nil {
			return nil, err
		}
		native.SetGitHubBatch(func(ctx context.Context, task tracker.GitHubBatchTask) error {
			scheduler.startGitHubBatch(ctx, native, task)
			return nil
		})
		connector, err := NewNativeConnector(native)
		if err != nil {
			return nil, fmt.Errorf("create native connector: %w", err)
		}
		scheduler.nativeProjects[project] = connector
	}
	return scheduler, nil
}

func (s *Scheduler) HeartbeatInterval() time.Duration {
	if s == nil {
		return 0
	}
	return s.heartbeatInterval
}

func (s *Scheduler) WorkspaceStorageConstraint(ctx context.Context) string {
	if s.storageConstraint == nil {
		return ""
	}
	return s.storageConstraint(ctx)
}

func (s *Scheduler) FetchCandidateIssues(ctx context.Context, request orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	source := s.nativeProject(request.ProjectID)
	if source == nil {
		return nil, fmt.Errorf("project %q is not mapped to a Hub-native project; generic Hub tracker mode was removed", request.ProjectID)
	}
	if err := s.CheckProjectPolicy(ctx, request.ProjectID, request.Repository, request.Policy); err != nil {
		return nil, schedulingError(err)
	}
	return s.fetchNativeCandidate(ctx, request, source)
}

func (s *Scheduler) PrepareProject(ctx context.Context, project string) error {
	if s.prepareProject == nil {
		return nil
	}
	err := s.prepareProject(ctx, project)
	var declared *bool
	if s.runnerSetupDeclared != nil {
		declared = s.runnerSetupDeclared(ctx, project)
	}
	s.mu.Lock()
	checks, reported := s.localChecks[project]
	previous := checks.Setup
	declarationChanged := false
	if reported {
		if s.runnerSetupDeclared != nil {
			previousDeclaration := checks.RunnerSetupDeclared
			declarationChanged = (previousDeclaration == nil) != (declared == nil) || previousDeclaration != nil && declared != nil && *previousDeclaration != *declared
			checks.RunnerSetupDeclared = declared
		}
		checks.Setup = "passed"
		if err != nil {
			checks.Setup = "failed"
		}
		s.localChecks[project] = checks
	}
	s.mu.Unlock()
	if source := s.nativeProject(project); source != nil && reported && (previous != checks.Setup || declarationChanged) {
		s.mu.Lock()
		if !s.nativeHeartbeats[source.NativeClient().ProjectID()].IsZero() {
			s.nativeHeartbeats[source.NativeClient().ProjectID()] = s.now().Add(-s.heartbeatInterval)
		}
		s.mu.Unlock()
		if reportErr := s.ensureNativeMachine(ctx, source); reportErr != nil {
			return errors.Join(err, reportErr)
		}
	}
	return err
}

func (s *Scheduler) AdoptClaim(ctx context.Context, issue connector.Issue, _ time.Time) (orchestrator.Claimed, error) {
	s.mu.Lock()
	lease, ok := s.claims[strings.TrimSpace(issue.ID)]
	if native, exists := s.nativeClaims[strings.TrimSpace(issue.ID)]; exists {
		issue.IsolationPolicy = native.lease.IsolationPolicy
	}
	s.mu.Unlock()
	if !ok {
		return orchestrator.Claimed{}, errors.New("hub claim was not found for candidate")
	}
	if err := s.checkClaimPolicy(ctx, issue.ID, lease.PolicyID); err != nil {
		return orchestrator.Claimed{}, err
	}
	return claimedIssue(issue, lease), nil
}

func (s *Scheduler) RenewClaim(ctx context.Context, issueID string, _ time.Time) (orchestrator.Claimed, error) {
	s.mu.Lock()
	native, isNative := s.nativeClaims[issueID]
	s.mu.Unlock()
	if isNative {
		if native.lifecycle != nil {
			native.lifecycle.Lock()
			defer native.lifecycle.Unlock()
			s.mu.Lock()
			current, found := s.nativeClaims[issueID]
			s.mu.Unlock()
			if !found || current.lifecycle != native.lifecycle {
				return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
			}
			native = current
		}
		return s.renewNativeClaim(ctx, issueID, native, 0)
	}
	return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
}

func (s *Scheduler) RenewClaimHeartbeat(ctx context.Context, issueID string, now time.Time) (orchestrator.Claimed, error) {
	s.mu.Lock()
	claim, ok := s.nativeClaims[issueID]
	s.mu.Unlock()
	if !ok {
		return s.RenewClaim(ctx, issueID, now)
	}
	if claim.lost {
		return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
	}
	active, err := s.nativeClaimRenewable(ctx, claim)
	if err != nil || !active {
		if err == nil {
			err = orchestrator.ErrSchedulingClaimLost
		}
		return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
	}
	started := s.now()
	lease, err := claim.source.NativeClient().RenewHeartbeat(ctx, claim.lease, int64(s.leaseTTL/time.Second))
	s.mu.Lock()
	current, found := s.nativeClaims[issueID]
	if found && !current.lost && current.lease.FencingToken != claim.lease.FencingToken {
		s.mu.Unlock()
		return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(current.lease)), nil
	}
	if err == nil && found && !current.lost && current.lease.ID == lease.ID && current.lease.FencingToken == lease.FencingToken {
		if current.lease.RenewedAt.After(lease.RenewedAt) {
			s.mu.Unlock()
			return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(current.lease)), nil
		}
		current.lease = lease
		current.deadline = nativeLeaseDeadline(started, lease)
		s.nativeClaims[issueID] = current
		s.claims[issueID] = nativeTrackerLease(lease)
		s.mu.Unlock()
		return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(lease)), nil
	}
	s.mu.Unlock()
	if err == nil {
		return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
	}
	if claim.lifecycle != nil {
		if !claim.lifecycle.TryLock() {
			return orchestrator.Claimed{}, errors.Join(ErrUnavailable, err)
		}
		defer claim.lifecycle.Unlock()
	}
	s.mu.Lock()
	current, found = s.nativeClaims[issueID]
	s.mu.Unlock()
	if found && !current.lost && current.lease.FencingToken != claim.lease.FencingToken {
		return claimedIssue(connector.Issue{ID: issueID}, nativeTrackerLease(current.lease)), nil
	}
	return orchestrator.Claimed{}, s.nativeClaimError(ctx, issueID, claim.lease.FencingToken, err)
}

func (s *Scheduler) ReleaseClaim(ctx context.Context, issueID string, reason string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	issueID = strings.TrimSpace(issueID)
	s.mu.Lock()
	native, isNative := s.nativeClaims[issueID]
	lease, ok := s.claims[issueID]
	if isNative && native.execution != nil {
		native.execution.RetainCompletion()
	}
	s.mu.Unlock()
	if !ok || !isNative {
		return runner.ErrExecutionAuthorityUnavailable
	}
	var err error
	if native.execution != nil {
		err = native.execution.FinishPrepared(ctx, reason)
		var rejection interface{ CompletionRejected() }
		if err != nil && !errors.As(err, &rejection) {
			return err
		}
	}
	if native.execution == nil {
		err = native.source.NativeClient().ReleaseTerminal(ctx, native.lease, "released")
	}
	var rejection interface{ CompletionRejected() }
	if err != nil && !claimLost(err) && !errors.As(err, &rejection) {
		return err
	}
	s.mu.Lock()
	if current, ok := s.claims[issueID]; ok && current.FencingToken != lease.FencingToken {
		s.mu.Unlock()
		return nil
	}
	delete(s.claims, issueID)
	delete(s.nativeClaims, issueID)
	delete(s.claimPolicies, issueID)
	s.mu.Unlock()
	if rejection != nil {
		return err
	}
	return nil
}

func schedulingError(err error) error { return claim.SchedulingError(err) }

func randomSessionID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (s *Scheduler) SetClaimAdmission(admission func() (func(), bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimAdmission = admission
}

func (s *Scheduler) SetUpdateOwner(owner func(context.Context, *runnerauth.UpdateRequest) *runnerauth.UpdateObservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateOwner = owner
}

func (s *Scheduler) SetProjectConfigurationOwner(owner func(context.Context, string, *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projectConfiguration = owner
}

func (s *Scheduler) nativeProject(project string) *NativeConnector {
	s.projectsMu.RLock()
	defer s.projectsMu.RUnlock()
	return s.nativeProjects[project]
}

func (s *Scheduler) nativeProjectSnapshot() map[string]*NativeConnector {
	s.projectsMu.RLock()
	defer s.projectsMu.RUnlock()
	return maps.Clone(s.nativeProjects)
}

func (s *Scheduler) SetNativeProjects(organization tracker.OrganizationID, projects map[string]tracker.ProjectID, checks map[string]runnerauth.LocalChecks) error {
	s.projectsMu.Lock()
	defer s.projectsMu.Unlock()
	for name := range s.nativeProjects {
		if _, allowed := projects[name]; !allowed {
			delete(s.nativeProjects, name)
		}
	}
	for name, id := range projects {
		if current := s.nativeProjects[name]; current != nil && current.NativeClient().ProjectID() == id {
			continue
		}
		native, err := s.client.Native(organization, id)
		if err != nil {
			return err
		}
		native.SetGitHubBatch(func(ctx context.Context, task tracker.GitHubBatchTask) error {
			s.startGitHubBatch(ctx, native, task)
			return nil
		})
		connector, err := NewNativeConnector(native)
		if err != nil {
			return fmt.Errorf("create native connector: %w", err)
		}
		s.nativeProjects[name] = connector
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, value := range checks {
		previous, ok := s.localChecks[name]
		if value.CommitSigning == "" {
			value.CommitName, value.CommitEmail, value.CommitSigning = previous.CommitName, previous.CommitEmail, previous.CommitSigning
		}
		if !ok || !reflect.DeepEqual(previous, value) {
			if s.localChecks == nil {
				s.localChecks = make(map[string]runnerauth.LocalChecks)
			}
			s.localChecks[name] = value
			delete(s.nativeHeartbeats, projects[name])
			delete(s.configHeartbeats, projects[name])
		}
	}
	return nil
}
