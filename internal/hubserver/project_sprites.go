package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// checkProjectSprites is the internal use path: unseal for one fixed provider
// call, audit before the call, and clear the byte buffer on every exit. No API
// or coordinator tool exposes a plaintext reader or an arbitrary callback.
func (s *Service) checkProjectSprites(ctx context.Context, scope nativeScope) (string, error) {
	scope, _, envelope, err := s.resolveSpritesSecret(ctx, nil, scope)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, corestore.ErrNotFound) {
		return "", err
	}
	if err != nil {
		return "", err
	}
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return "", err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
	if err != nil {
		return "", err
	}
	defer clear(token)
	return validateSpritesToken(ctx, s.config.SpritesHTTPClient, token)
}

// spriteRunnerIdle is how long a runner may go without a heartbeat before the
// Hub treats its Sprite as paused. Runners heartbeat every second while awake.
const spriteRunnerIdle = 15 * time.Second

type spriteWakeKey struct {
	organization tracker.OrganizationID
}

type spriteLifecyclePass struct {
	cancel       context.CancelFunc
	done         chan struct{}
	pendingState string
}

func (s *Service) scheduleSpriteWake(ctx context.Context, scope nativeScope, result json.RawMessage) {
	if s.config.SecretKeys == nil {
		return
	}
	var issue struct {
		State string `json:"state"`
	}
	if json.Unmarshal(result, &issue) != nil || issue.State == "" {
		return
	}
	dispatchable, err := s.spriteWakeDispatchable(ctx, scope, issue.State)
	if err != nil || !dispatchable {
		return
	}
	s.scheduleSpriteLifecycle(ctx, scope, issue.State)
}

func (s *Service) scheduleSpriteLifecycle(ctx context.Context, scope nativeScope, state string) {
	if s.config.SecretKeys == nil {
		return
	}
	if state != "" {
		allowed, err := monthlySpriteAllowed(ctx, nil, nativeScope{organization: scope.organization}, s.config.now(), s.database.billingRepository)
		if err != nil {
			s.config.Logger.Warn("Check Sprite lifecycle monthly budget", "error", err)
			return
		}
		if !allowed {
			return
		}
	}
	key := spriteWakeKey{organization: scope.organization}
	s.spriteWakeMu.Lock()
	defer s.spriteWakeMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if active := s.spriteWakes[key]; active != nil {
		if state != "" {
			active.pendingState = state
		}
		return
	}
	wakeContext, cancel := context.WithTimeout(s.freshRunDetailContext(ctx), runnerauth.MaxEnrollmentTTL)
	pass := &spriteLifecyclePass{done: make(chan struct{}), cancel: cancel}
	s.spriteWakes[key] = pass
	s.spriteWakeWork.Add(1)
	go func() {
		defer s.spriteWakeWork.Done()
		defer func() {
			s.spriteWakeMu.Lock()
			pending := pass.pendingState
			delete(s.spriteWakes, key)
			close(pass.done)
			s.spriteWakeMu.Unlock()
			if pending != "" && ctx.Err() == nil {
				s.scheduleSpriteLifecycle(ctx, scope, pending)
			}
		}()
		defer cancel()
		if state != "" {
			if _, err := s.wakeOrganizationSpriteRunners(wakeContext, scope); err != nil {
				if wakeContext.Err() == nil {
					s.config.Logger.Warn("sprite runners could not be woken", "error", err)
				}
				return
			}
		}
		if err := s.scaleSpritePool(wakeContext, scope, state != ""); err != nil && wakeContext.Err() == nil {
			s.config.Logger.Warn("Sprite pool lifecycle failed", "organization", scope.organization, "project", scope.project, "error", err)
		}
	}()
}

func (s *Service) stopSpriteRunners() {
	s.spriteWakeMu.Lock()
	s.workerCancel()
	s.spriteWakeMu.Unlock()
	s.spriteWakeWork.Wait()
}

func (s *Service) spriteWakeDispatchable(ctx context.Context, scope nativeScope, state string) (bool, error) {
	dispatchable := false
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		for _, candidate := range project.States {
			if candidate.Name == state && candidate.Dispatchable && !candidate.Terminal {
				dispatchable = true
			}
		}
		return nil
	})
	return dispatchable, err
}

type spriteWakeCandidate struct {
	runnerID    string
	machineID   tracker.MachineID
	name        string
	secretScope nativeScope
	envelope    hubsecrets.Envelope
}

func (s *Service) wakeSpriteRunners(ctx context.Context, scope nativeScope, state string) (int, error) {
	if s.config.SecretKeys == nil {
		return 0, nil
	}
	client := http.Client{Timeout: 45 * time.Second}
	if s.config.SpritesHTTPClient != nil {
		client = *s.config.SpritesHTTPClient
		client.Timeout = 45 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	woken := 0
	last := ""
	for {
		candidate, found, err := s.nextSpriteWakeCandidate(ctx, scope, state, &last)
		if err != nil || !found {
			return woken, err
		}
		started, err := s.wakeSpriteRunnerWithSecret(ctx, scope, candidate.secretScope, &client, candidate.name, candidate.envelope)
		if err != nil {
			return woken, err
		}
		failedAt := ""
		if !started {
			failedAt = hubapi.FormatHubTime(s.config.now())
		}
		if err := s.recordSpriteWakeFacts(ctx, scope, candidate, failedAt, started); err != nil {
			return woken, err
		}
		if started {
			woken++
		}
	}
}

func (s *Service) nextSpriteWakeCandidate(ctx context.Context, scope nativeScope, state string, last *string) (spriteWakeCandidate, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	dispatchable, err := s.spriteWakeDispatchable(ctx, scope, state)
	if err != nil || !dispatchable {
		return spriteWakeCandidate{}, false, err
	}
	now := s.config.now()
	configured := false
	var runners []placementRunner
	err = s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		_, err := owner.SpritePool(ctx)
		configured = err == nil
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		runners, err = readPlacementRunners(ctx, r, scope, now)
		return err
	})
	if err != nil {
		return spriteWakeCandidate{}, false, err
	}
	var placement placementSnapshot
	if configured {
		placement, err = s.readPlacementSnapshot(ctx, scope, now, nil)
		if err != nil {
			return spriteWakeCandidate{}, false, err
		}
		if placement.Provisionable == 0 {
			return spriteWakeCandidate{}, false, nil
		}
	}
	slices.SortFunc(runners, func(a, b placementRunner) int {
		if n := strings.Compare(a.Hostname, b.Hostname); n != 0 {
			return n
		}
		return strings.Compare(a.RunnerID, b.RunnerID)
	})
	for _, runner := range runners {
		if runner.Hostname <= *last || !runner.Sprite || runner.State != "active" || runner.Health == "revoked" || runner.Health == "expired" || runner.Pending || !runner.LastHeartbeatAt.Before(now.Add(-spriteRunnerIdle)) {
			continue
		}
		if runner.Scope != "organization" && !slices.Contains(runner.ProjectIDs, scope.project) {
			continue
		}
		*last = runner.Hostname
		if configured {
			compatible := false
			for _, r := range placement.Runners {
				if r.RunnerID != runner.RunnerID || !r.Sprite || r.Pending || !placementRunnerCompatible(r, scope.project, placement.Requirements, now, true) {
					continue
				}
				for id, requirement := range placement.Items {
					if placement.Policy.Mode == "local_first" && placement.Local[id] > 0 {
						continue
					}
					if slices.ContainsFunc(r.ProviderCapacity, func(view providercapacity.View) bool { return view.Available() && view.Supports(requirement) }) {
						compatible = true
					}
				}
			}
			if !compatible {
				continue
			}
		}
		candidate := spriteWakeCandidate{runnerID: runner.RunnerID, machineID: runner.MachineID, name: runner.Hostname}
		candidate.secretScope, candidate.envelope, err = s.spriteWakeSecret(ctx, nil, scope, candidate.name)
		if errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return spriteWakeCandidate{}, false, err
		}
		return candidate, true, nil
	}
	return spriteWakeCandidate{}, false, nil
}

func (s *Service) spriteWakeSecret(ctx context.Context, _ hubapi.NativeQueryer, scope nativeScope, name string) (nativeScope, hubsecrets.Envelope, error) {
	var provider string
	managed := false
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		member, err := owner.SpriteMember(ctx, name)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if member.State == "deleted" {
			return nil
		}
		managed = true
		scope.project, provider = tracker.ProjectID(member.TokenProjectID), member.ProviderOrganization
		return nil
	})
	if err != nil {
		return nativeScope{}, hubsecrets.Envelope{}, err
	}
	secretScope, slug, envelope, err := s.resolveSpritesSecret(ctx, nil, scope)
	if err != nil {
		return nativeScope{}, hubsecrets.Envelope{}, err
	}
	if managed && slug != provider {
		return nativeScope{}, hubsecrets.Envelope{}, corestore.ErrNotFound
	}
	return secretScope, envelope, nil
}

func (s *Service) wakeSpriteRunnerWithSecret(ctx context.Context, scope, secretScope nativeScope, client *http.Client, name string, envelope hubsecrets.Envelope) (bool, error) {
	allowed, err := monthlySpriteAllowed(ctx, nil, scope, s.config.now(), s.database.billingRepository)
	if err != nil || !allowed {
		return false, err
	}
	if err := s.auditSecretUse(ctx, secretScope, envelope.Version); err != nil {
		return false, err
	}
	token, err := s.config.SecretKeys.Open(envelope, secretAAD(string(secretScope.organization), string(secretScope.project), flySpritesToken))
	if err != nil {
		return false, err
	}
	defer clear(token)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.sprites.dev/v1/sprites/"+name+"/services/detent-runner/start?duration=30s", nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	response, err := client.Do(request)
	request.Header.Del("Authorization")
	if err != nil {
		return false, nil
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	return readErr == nil && response.StatusCode >= 200 && response.StatusCode <= 299, nil
}

func (s *Service) wakeOrganizationSpriteRunners(ctx context.Context, scope nativeScope) (int, error) {
	type target struct {
		project tracker.ProjectID
		state   string
		rank    int64
	}
	var targets []target
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		after := tracker.ProjectID("")
		for {
			page, err := r.Projects(ctx, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, project := range page {
				if project.Profile != "native" || project.DeletedAt != nil {
					continue
				}
				record, err := r.ProjectRecord(ctx, project.ID)
				if err != nil {
					return err
				}
				for _, state := range record.States {
					if !state.Dispatchable || state.Terminal {
						continue
					}
					candidates, err := r.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: project.ID, Scope: string(project.ID), States: []string{state.Name}, AvailableAt: s.config.now(), Limit: 1})
					if err != nil {
						return err
					}
					if len(candidates.Items) > 0 {
						targets = append(targets, target{project: project.ID, state: state.Name, rank: project.SchedulingRank})
					}
				}
			}
			if len(page) < corestore.MaxPageSize {
				break
			}
			next := page[len(page)-1].ID
			if next <= after {
				return corestore.ErrInvalidCursor
			}
			after = next
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	slices.SortFunc(targets, func(a, b target) int {
		if a.rank < b.rank {
			return -1
		}
		if a.rank > b.rank {
			return 1
		}
		if n := strings.Compare(string(a.project), string(b.project)); n != 0 {
			return n
		}
		return strings.Compare(a.state, b.state)
	})
	total := 0
	for _, target := range targets {
		scope.project = target.project
		count, err := s.wakeSpriteRunners(ctx, scope, target.state)
		if err != nil {
			return total, err
		}
		total += count
	}
	return total, nil
}

func (s *Service) recordSpriteWakeFacts(ctx context.Context, scope nativeScope, candidate spriteWakeCandidate, failedAt string, started bool) error {
	now := s.config.now()
	update := leasestore.SpriteWake{MachineID: string(candidate.machineID), RunnerID: candidate.runnerID, Name: candidate.name, ObservedAt: now}
	if failedAt != "" {
		value, err := time.Parse(time.RFC3339Nano, failedAt)
		if err != nil {
			return err
		}
		update.FailedAt = &value
	}
	if started {
		update.WokenAt = &now
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error { return w.UpdateMachineSpriteWake(ctx, update) })
}

func spriteFactsForMachine(machine leasestore.Machine) machineSpriteFacts {
	var facts machineSpriteFacts
	if machine.SpriteName != "" {
		facts.Name = new(machine.SpriteName)
	}
	if machine.SpriteWokenAt != nil {
		facts.WokenAt = new(hubapi.FormatHubTime(*machine.SpriteWokenAt))
	}
	if machine.SpriteWakeFailedAt != nil {
		facts.WakeFailedAt = new(hubapi.FormatHubTime(*machine.SpriteWakeFailedAt))
	}
	return facts
}

func (s *Service) readSpriteMachineFacts(ctx context.Context, organization tracker.OrganizationID, machine tracker.MachineID) (machineSpriteFacts, error) {
	var result machineSpriteFacts
	err := s.accessRead(ctx, organization, func(r tenantstore.Reader) error {
		row, err := r.Machine(ctx, string(machine))
		if err == nil {
			result = spriteFactsForMachine(row)
		}
		return err
	})
	return result, err
}
