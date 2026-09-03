package hubclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const hubWorkItemField = "detent_hub_work_item_id"

type SchedulerConfig struct {
	Machine           Machine
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	Now               func() time.Time
	SessionID         func() (string, error)
}

type Scheduler struct {
	client            *Client
	machine           Machine
	heartbeatInterval time.Duration
	leaseTTL          time.Duration
	now               func() time.Time
	sessionID         func() (string, error)
	mu                sync.Mutex
	registered        bool
	lastHeartbeat     time.Time
	claims            map[string]tracker.Lease
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
	return &Scheduler{
		client: client, machine: config.Machine, heartbeatInterval: config.HeartbeatInterval,
		leaseTTL: config.LeaseTTL, now: now, sessionID: sessionID, claims: make(map[string]tracker.Lease),
	}, nil
}

func (s *Scheduler) HeartbeatInterval() time.Duration {
	if s == nil {
		return 0
	}
	return s.heartbeatInterval
}

func (s *Scheduler) FetchCandidateIssues(ctx context.Context, request orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	if err := s.ensureMachine(ctx); err != nil {
		return nil, schedulingError(err)
	}
	sessionID, err := s.sessionID()
	if err != nil {
		return nil, fmt.Errorf("create Hub claim session: %w", err)
	}
	claimRequest := ClaimRequest{
		MachineID: s.machine.ID, SessionID: sessionID, TTLSeconds: int64(s.leaseTTL / time.Second),
		WorkflowState: append([]string(nil), request.WorkflowStates...),
		Authors:       append([]string(nil), request.Filter.Authors...),
		Assignees:     append([]string(nil), request.Filter.Assignees...),
		LabelInclude:  append([]string(nil), request.Filter.LabelInclude...),
		LabelExclude:  append([]string(nil), request.Filter.LabelExclude...),
	}
	if repository := strings.TrimSpace(request.Repository); repository != "" {
		claimRequest.Repositories = []string{repository}
	}
	lease, err := s.client.Claim(ctx, claimRequest)
	if errors.Is(err, ErrNoClaimableWork) {
		return []connector.Issue{}, nil
	}
	if err != nil {
		return nil, schedulingError(err)
	}
	item, err := s.client.WorkItem(ctx, lease.WorkItemID)
	if err != nil {
		return nil, s.releaseAfterCandidateFailure(ctx, lease, "work_item_hydration_failed", schedulingError(err))
	}
	issue := issueFromWorkItem(item)
	if issue.ID == "" {
		return nil, s.releaseAfterCandidateFailure(ctx, lease, "work_item_identity_missing", errors.New("hub work item has no GitHub node ID"))
	}
	s.mu.Lock()
	s.claims[issue.ID] = lease
	s.mu.Unlock()
	return []connector.Issue{issue}, nil
}

func (s *Scheduler) AdoptClaim(_ context.Context, issue connector.Issue, _ time.Time) (orchestrator.Claimed, error) {
	s.mu.Lock()
	lease, ok := s.claims[strings.TrimSpace(issue.ID)]
	s.mu.Unlock()
	if !ok {
		return orchestrator.Claimed{}, errors.New("hub claim was not found for candidate")
	}
	return claimedIssue(issue, lease), nil
}

func (s *Scheduler) releaseAfterCandidateFailure(ctx context.Context, lease tracker.Lease, reason string, cause error) error {
	if err := s.client.Release(context.WithoutCancel(ctx), lease, reason); err != nil {
		return errors.Join(cause, fmt.Errorf("release Hub claim after %s: %w", reason, err))
	}
	return cause
}

func (s *Scheduler) RenewClaim(ctx context.Context, issueID string, _ time.Time) (orchestrator.Claimed, error) {
	if err := s.ensureMachine(ctx); err != nil {
		return orchestrator.Claimed{}, err
	}
	s.mu.Lock()
	lease, ok := s.claims[strings.TrimSpace(issueID)]
	s.mu.Unlock()
	if !ok {
		return orchestrator.Claimed{}, orchestrator.ErrSchedulingClaimLost
	}
	renewed, err := s.client.Renew(ctx, lease, s.leaseTTL)
	if err != nil {
		if claimLost(err) {
			s.mu.Lock()
			delete(s.claims, strings.TrimSpace(issueID))
			s.mu.Unlock()
			return orchestrator.Claimed{}, errors.Join(orchestrator.ErrSchedulingClaimLost, err)
		}
		return orchestrator.Claimed{}, err
	}
	s.mu.Lock()
	s.claims[strings.TrimSpace(issueID)] = renewed
	s.mu.Unlock()
	issue := connector.Issue{ID: strings.TrimSpace(issueID)}
	return claimedIssue(issue, renewed), nil
}

func (s *Scheduler) ReleaseClaim(ctx context.Context, issueID string, reason string) error {
	issueID = strings.TrimSpace(issueID)
	s.mu.Lock()
	lease, ok := s.claims[issueID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	err := s.client.Release(ctx, lease, reason)
	if err != nil && !claimLost(err) {
		return err
	}
	s.mu.Lock()
	delete(s.claims, issueID)
	s.mu.Unlock()
	return nil
}

func (s *Scheduler) ensureMachine(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if !s.registered {
		registered, err := s.client.RegisterMachine(ctx, s.machine)
		if err != nil {
			return err
		}
		s.machine = registered
		s.registered = true
		s.lastHeartbeat = now
		return nil
	}
	if now.Before(s.lastHeartbeat.Add(s.heartbeatInterval)) {
		return nil
	}
	displayName := s.machine.DisplayName
	capabilities := s.machine.Capabilities
	capacity := s.machine.Capacity
	version := s.machine.Version
	updated, err := s.client.HeartbeatMachine(ctx, s.machine.ID, MachineHeartbeat{
		DisplayName: &displayName, Capabilities: &capabilities, Capacity: &capacity, Version: &version,
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == "machine_not_found" {
			s.registered = false
			registered, registerErr := s.client.RegisterMachine(ctx, s.machine)
			if registerErr != nil {
				return registerErr
			}
			s.machine = registered
			s.registered = true
			s.lastHeartbeat = now
			return nil
		}
		return err
	}
	s.machine = updated
	s.lastHeartbeat = now
	return nil
}

func issueFromWorkItem(item WorkItem) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = strings.TrimSpace(item.GitHub.NodeID)
	issue.Identifier = strings.TrimSpace(item.Repository.Owner) + "/" + strings.TrimSpace(item.Repository.Name) + "#" + strconv.Itoa(item.GitHub.Number)
	issue.Number = item.GitHub.Number
	issue.Title = item.Title
	issue.Description = strings.TrimSpace(item.Body)
	if issue.Description == "" {
		issue.Description = item.BodyExcerpt
	}
	issue.URL = item.URL
	issue.Closed = item.SourceState == tracker.SourceStateClosed
	issue.AuthorID = item.AuthorID
	issue.Labels = append([]string(nil), item.Labels...)
	issue.Assignees = append([]string(nil), item.Assignees...)
	issue.CreatedAt = cloneTime(item.CreatedAt)
	issue.UpdatedAt = cloneTime(item.UpdatedAt)
	issue.AssignedToWorker = true
	issue.Fields[hubWorkItemField] = strconv.FormatInt(int64(item.ID), 10)
	issue.Metadata[hubWorkItemField] = strconv.FormatInt(int64(item.ID), 10)
	issue.Metadata["hub_sync_status"] = string(item.SyncStatus)
	if item.WorkflowState != nil {
		issue.State = item.WorkflowState.Name
	}
	if item.Queue != nil && item.Queue.PriorityRank != nil {
		priority := *item.Queue.PriorityRank
		issue.Priority = &priority
		issue.PriorityName = queuePriorityName(priority)
	}
	for _, blocker := range item.Blockers {
		state := ""
		if blocker.WorkflowState != nil {
			state = blocker.WorkflowState.Name
		}
		issue.BlockedBy = append(issue.BlockedBy, connector.BlockedRef{
			ID: strconv.FormatInt(int64(blocker.ID), 10), Identifier: blocker.Repository + "#" + strconv.Itoa(blocker.IssueNumber),
			State: state, TrackerState: string(blocker.SourceState), Source: connector.BlockedRefSourceNative,
		})
	}
	if len(item.PullRequests) > 0 {
		pull := item.PullRequests[0]
		issue.PRNumber = intPointer(pull.Number)
		issue.PRRepository = strings.TrimSpace(item.Repository.Owner) + "/" + strings.TrimSpace(item.Repository.Name)
		issue.PullRequest = &connector.PullRequest{
			NodeID: pull.GitHubNodeID, Number: pull.Number, URL: pull.URL, BranchName: pull.HeadRef,
			BaseRef: pull.BaseRef, State: pull.State, Draft: pull.Draft, HeadSHA: pull.HeadSHA, BaseSHA: pull.BaseSHA,
			CIStatus: pull.Checks.Status, CheckRunCount: pull.Checks.Total, CodexReviewState: pull.Reviews.Decision,
		}
	}
	return issue
}

func claimedIssue(issue connector.Issue, lease tracker.Lease) orchestrator.Claimed {
	if issue.Metadata == nil {
		issue.Metadata = make(map[string]string)
	}
	issue.Metadata["hub_lease_id"] = string(lease.ID)
	issue.Metadata["hub_fencing_token"] = strconv.FormatInt(int64(lease.FencingToken), 10)
	issue.Metadata["hub_session_id"] = lease.SessionID
	return orchestrator.Claimed{
		Issue: issue, ClaimedAt: lease.AcquiredAt, Owner: string(lease.Machine.ID),
		LeaseRenewedAt: lease.RenewedAt, LeaseExpiresAt: lease.ExpiresAt,
	}
}

func claimLost(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == "stale_fencing_token" || apiErr.Code == "lease_not_found" || apiErr.Code == "lease_conflict"
}

func schedulingError(err error) error {
	if err == nil || !errors.Is(err, ErrUnavailable) {
		return err
	}
	return errors.Join(orchestrator.ErrSchedulingUnavailable, err)
}

func queuePriorityName(priority int) string {
	switch priority {
	case tracker.QueuePriorityUrgent:
		return "urgent"
	case tracker.QueuePriorityHigh:
		return "high"
	case tracker.QueuePriorityNormal:
		return "normal"
	case tracker.QueuePriorityLow:
		return "low"
	default:
		return ""
	}
}

func randomSessionID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func intPointer(value int) *int {
	return &value
}
