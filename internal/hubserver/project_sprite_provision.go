package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) poolSpriteToken(ctx context.Context, scope hubscope.NativeScope, name string) ([]byte, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	var secret accessstore.Secret
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(readCtx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		actor, err := owner.SpritePoolAuthority(readCtx, s.config.now())
		if err != nil {
			return err
		}
		scope.Credential.ID = actor
		member, err := owner.SpriteMember(readCtx, name)
		if err != nil {
			return err
		}
		if member.State == "deleted" {
			return corestore.ErrNotFound
		}
		scope.Project = tracker.ProjectID(member.TokenProjectID)
		secret, err = r.AccessSecret(readCtx, scope.Project, flySpritesToken)
		if errors.Is(err, corestore.ErrNotFound) && scope.Project != "" {
			secret, err = r.AccessSecret(readCtx, "", flySpritesToken)
		}
		if err != nil {
			return err
		}
		if secret.OrganizationSlug != member.ProviderOrganization {
			return errSpritesValidation
		}
		scope.Project = secret.ProjectID
		return accessstore.VerifySecret(s.config.SecretKeys, secret)
	})
	if err != nil {
		return nil, err
	}
	if err := s.auditSecretUse(ctx, scope, secret.Envelope.Version); err != nil {
		return nil, err
	}
	return s.config.SecretKeys.Open(secret.Envelope, secretAAD(string(scope.Organization), string(scope.Project), flySpritesToken))
}

func (s *Service) spritePoolClient() *http.Client {
	client := http.Client{}
	if s.config.SpritesHTTPClient != nil {
		client = *s.config.SpritesHTTPClient
	}
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (s *Service) poolSpriteRequest(ctx context.Context, scope hubscope.NativeScope, name, method, path string, body []byte) (*http.Response, error) {
	if method != http.MethodDelete && method != http.MethodGet {
		allowed, err := monthlySpriteAllowed(ctx, nil, scope, s.config.now(), s.database.billingRepository)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, providerWait("provider_capacity", "Monthly Sprite budget is exhausted")
		}
	}
	token, err := s.poolSpriteToken(ctx, scope, name)
	if err != nil {
		return nil, err
	}
	defer clear(token)
	request, err := http.NewRequestWithContext(ctx, method, "https://api.sprites.dev/v1/sprites"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	defer request.Header.Del("Authorization")
	request.Header.Set("Content-Type", "application/json")
	response, err := s.spritePoolClient().Do(request)
	if err != nil {
		return nil, errSpritesValidation
	}
	return response, nil
}

func (s *Service) createPoolSprite(ctx context.Context, scope hubscope.NativeScope, settings spritePoolSettings, allowOrganization bool) (bool, error) {
	return s.createPoolSpriteWithNames(ctx, scope, settings, allowOrganization, &spriteNameGenerator{intN: rand.IntN})
}

func (s *Service) createPoolSpriteWithNames(ctx context.Context, scope hubscope.NativeScope, settings spritePoolSettings, allowOrganization bool, names *spriteNameGenerator) (bool, error) {
	if s.config.Hosted == nil || settings.MaxRunners == 0 {
		return false, nil
	}
	if _, err := s.poolSpriteTokenAvailable(ctx, scope); err != nil {
		return false, err
	}
	created := false
	for {
		name, err := names.next(func(name string) (bool, error) {
			readCtx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
			defer cancel()
			var exists bool
			err := s.accessRead(readCtx, scope.Organization, func(r tenantstore.Reader) error {
				owner, ok := r.(postgres.SpriteReader)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				_, err := owner.SpriteMember(readCtx, name)
				exists = err == nil
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				return err
			})
			return exists, err
		})
		if err != nil {
			return created, err
		}
		reserved, err := s.createPoolSpriteNamed(ctx, scope, settings, allowOrganization, name)
		created = created || reserved
		if !errors.Is(err, errSpriteNameTaken) {
			return created, err
		}
	}
}

func (s *Service) createPoolSpriteNamed(ctx context.Context, scope hubscope.NativeScope, settings spritePoolSettings, allowOrganization bool, name string) (bool, error) {
	scope.Project = ""
	now := s.config.now()
	placement, err := s.readOrganizationPlacementSnapshot(ctx, scope, now)
	if err != nil {
		return false, err
	}
	projects, err := s.readSpriteAllocationProjects(ctx, scope)
	if err != nil {
		return false, err
	}
	allocationRevision := tracker.Revision(0)
	for _, project := range projects {
		selected := scope
		selected.Project = project
		allocation, err := s.readSpriteAllocationController(ctx, selected)
		if err != nil {
			return false, err
		}
		if allocation.Shortfall <= 0 {
			continue
		}
		valid := false
		err = s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
			var err error
			valid, err = s.projectSpriteAllocationAuthority(ctx, r, selected)
			return err
		})
		if err != nil {
			return false, err
		}
		if !valid {
			continue
		}
		decision, err := checkMonthlyBudget(ctx, nil, selected, budget.CostExposure{SpriteInfrastructure: true}, false, now, s.config.TenantRepository)
		if err != nil {
			return false, err
		}
		if !decision.Allowed {
			continue
		}
		scope.Project = project
		settings.MinRunners = allocation.DesiredCount
		allocationRevision = allocation.Revision
		break
	}
	if scope.Project == "" && !allowOrganization {
		return false, nil
	}
	token, err := s.config.deps.GenerateToken()
	if err != nil {
		return false, err
	}
	enrollment := runnerauth.Enrollment{ID: hubapi.NewID("enrollment"), Token: token, ExpiresAt: now.Add(runnerauth.MaxEnrollmentTTL)}
	reserved := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		actor, err := owner.SpritePoolAuthority(ctx, s.config.now())
		if err != nil {
			return err
		}
		scope.Credential.ID = actor
		pool, err := owner.SpritePool(ctx)
		if err != nil {
			return err
		}
		if pool.Revision != settings.Revision {
			return nil
		}
		members, err := readSpriteMembers(ctx, owner)
		if err != nil {
			return err
		}
		count, ownerCount := 0, 0
		for _, member := range members {
			if member.State != "deleted" {
				count++
				if member.AllocationProjectID == string(scope.Project) {
					ownerCount++
				}
			}
		}
		ceiling := int(pool.MaxRunners)
		if count >= ceiling {
			return nil
		}
		if _, err := owner.SpriteMember(ctx, name); err == nil {
			return errSpriteNameTaken
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if scope.Project != "" {
			allocation, _, err := projectSpriteAllocationRecord(ctx, w, scope)
			if err != nil {
				return err
			}
			if allocation.Revision != allocationRevision {
				return corestore.ErrRevisionConflict
			}
			valid, err := s.projectSpriteAllocationAuthority(ctx, w, scope)
			if err != nil {
				return err
			}
			if !valid {
				return nil
			}
			settings.MinRunners = allocation.DesiredCount
		}
		allowed, err := spriteMonthlyAllowedRecord(ctx, w, scope, s.config.now())
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		input := spriteScaleInput{Depth: placement.SpriteTarget, Free: placement.SpriteFree, Pending: placement.Pending, Floor: settings.MinRunners, Ceiling: ceiling, Count: ownerCount, CreateLimit: placement.ProviderFree}
		decision, err := spriteMonthlyDecisionRecord(ctx, w, scope, false, s.config.now())
		if err != nil {
			return err
		}
		if !decision.Allowed {
			input.CreateLimit = new(placement.Provisionable)
		}
		providerFree, known, err := spritePoolProviderFree(ctx, w, scope, count, s.config.now())
		if err != nil {
			return err
		}
		if scope.Project != "" {
			input.CreateLimit = nil
			input.Depth, input.Free, input.Pending = 0, 0, 0
		} else if placement.Policy.Mode == "local_first" {
			input.Floor = min(input.Floor, placement.Policy.OverflowSlots)
		}
		if known && (input.CreateLimit == nil || providerFree < *input.CreateLimit) {
			input.CreateLimit = new(providerFree)
		}
		if !decision.Allowed && scope.Project != "" {
			input.CreateLimit = new(0)
		}
		input.Ceiling = min(input.Ceiling, ownerCount+ceiling-count)
		if decideSpriteScale(input).Create == 0 {
			return nil
		}
		secret, err := w.AccessSecret(ctx, "", flySpritesToken)
		if err != nil {
			return err
		}
		if err := accessstore.VerifySecret(s.config.SecretKeys, secret); err != nil {
			return err
		}
		enrollmentScope := "organization"
		var projectIDs []tracker.ProjectID
		if scope.Project != "" {
			enrollmentScope = "projects"
			projectIDs = []tracker.ProjectID{scope.Project}
		}
		if err := w.CreateRunnerEnrollment(ctx, runnerstore.Enrollment{OrganizationID: scope.Organization, ID: enrollment.ID, TokenHash: apikey.HashToken(token), Scope: enrollmentScope, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, ProjectIDs: projectIDs, CreatedAt: now, ExpiresAt: enrollment.ExpiresAt, CreatedBy: actor}); err != nil {
			return err
		}
		if err := w.CreateRunnerSpriteMembership(ctx, runnerstore.SpriteMembership{OrganizationID: scope.Organization, Name: name, ProviderOrganization: secret.OrganizationSlug, EnrollmentID: enrollment.ID, State: "bootstrapping", IdleSince: now, CreatedAt: now, AllocationProjectID: scope.Project, AllocationRevision: int64(allocationRevision)}); errors.Is(err, corestore.ErrUniqueViolation) {
			return errSpriteNameTaken
		} else if err != nil {
			return err
		}
		if err := s.applyRunnerTenantUsage(ctx, w, now, enrollment.ID, false, false); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil || !reserved {
		return false, err
	}

	member := spritePoolMember{AllocationProjectID: scope.Project, Name: name, State: "bootstrapping", EnrollmentID: enrollment.ID}
	if err := s.requireSpriteMemberWanted(ctx, scope, name); err != nil {
		return true, s.cancelPoolSpriteReservation(ctx, scope, member, err)
	}
	err = s.bootstrapPoolSprite(ctx, scope, member, settings, enrollment)
	if err == nil {
		return true, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errors.Is(err, errSpriteNameTaken) {
		if cancelErr := s.cancelPoolSpriteReservation(cleanup, scope, member, err); cancelErr != nil {
			return true, cancelErr
		}
		return true, errSpriteNameTaken
	}
	stale := errors.Is(err, errSpriteReservationStale)
	failure := "Sprite provisioning did not complete; check customer setup and retry\n"
	if stale {
		failure = errSpriteReservationStale.Error() + "; its reservation was released\n"
	} else if errors.Is(err, errSpritesTokenRejected) {
		failure = errSpritesTokenRejected.Error() + "\n"
	} else if errors.Is(err, errSpritesBilling) {
		failure = errSpritesBilling.Error() + "\n"
	}
	markErr := s.retirePoolSpriteReservation(cleanup, scope, name, failure, !stale)
	if stale {
		err = nil
	}
	if ctx.Err() != nil {
		return true, errors.Join(err, markErr)
	}
	return true, errors.Join(err, markErr, s.deletePoolSprite(ctx, scope, member))
}

func (s *Service) requireSpriteMemberWanted(ctx context.Context, scope hubscope.NativeScope, name string) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var wanted bool
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		var err error
		wanted, err = spriteMemberWantedRecord(ctx, r, name)
		return err
	})
	if err == nil && !wanted {
		return errSpriteReservationStale
	}
	return err
}

func (s *Service) cancelPoolSpriteReservation(ctx context.Context, scope hubscope.NativeScope, member spritePoolMember, cause error) error {
	if !errors.Is(cause, errSpriteReservationStale) && !errors.Is(cause, errSpriteNameTaken) {
		return errors.Join(cause, s.retirePoolSpriteReservation(ctx, scope, member.Name, "Sprite provisioning did not start\n", false))
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		row, err := owner.SpriteMember(ctx, member.Name)
		if err != nil {
			return err
		}
		if row.EnrollmentID != member.EnrollmentID {
			return corestore.ErrRevisionConflict
		}
		if row.State != "bootstrapping" && row.State != "deleting" {
			return nil
		}
		enrollment, err := w.RunnerEnrollment(ctx, row.EnrollmentID)
		if err != nil {
			return err
		}
		if enrollment.RedeemedAt == nil && enrollment.RevokedAt == nil {
			if err := w.RevokeRunnerEnrollment(ctx, enrollment.ID, s.config.now()); err != nil {
				return err
			}
		}
		updated := spriteMembershipRecord(row)
		updated.State = "deleted"
		updated.BootstrapLog += cause.Error() + " before paid creation; its reservation was cancelled\n"
		return w.UpdateRunnerSpriteMembership(ctx, updated)
	})
}

func (s *Service) retirePoolSpriteReservation(ctx context.Context, scope hubscope.NativeScope, name, log string, failed bool) error {
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		row, err := owner.SpriteMember(ctx, name)
		if err != nil {
			return err
		}
		if row.State != "bootstrapping" && row.State != "enrolled" {
			return nil
		}
		updated := spriteMembershipRecord(row)
		updated.State = "deleting"
		updated.BootstrapLog += log
		if err := w.UpdateRunnerSpriteMembership(ctx, updated); err != nil {
			return err
		}
		if !failed || row.AllocationProjectID == "" {
			return nil
		}
		allocation, err := owner.SpriteAllocation(ctx, tracker.ProjectID(row.AllocationProjectID))
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if allocation.Revision != row.AllocationRevision {
			return nil
		}
		return owner.RecordSpriteProvisionFailure(ctx, tracker.ProjectID(row.AllocationProjectID), row.AllocationRevision)
	})
}

func (s *Service) poolSpriteTokenAvailable(ctx context.Context, scope hubscope.NativeScope) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { _, err := r.AccessSecret(ctx, "", flySpritesToken); return err })
	return err == nil, err
}

func (s *Service) bootstrapPoolSprite(ctx context.Context, scope hubscope.NativeScope, member spritePoolMember, settings spritePoolSettings, enrollment runnerauth.Enrollment) error {
	version := "v" + strings.TrimPrefix(s.config.Version, "v")
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return hubapi.NativePublicInvalid("Sprite provisioning requires a Hub built from a pinned release")
	}
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: member.Name})
	if err != nil {
		return err
	}
	response, err := s.poolSpriteRequest(ctx, scope, member.Name, http.MethodPost, "", body)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if response.StatusCode == http.StatusConflict {
		return errSpriteNameTaken
	}
	if response.StatusCode == http.StatusBadRequest && readErr == nil {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) == nil {
			message := strings.ToLower(failure.Error)
			if strings.Contains(message, "already exists") || strings.Contains(message, "already taken") {
				return errSpriteNameTaken
			}
		}
	}
	if response.StatusCode != http.StatusCreated {
		return spritesResponseError(response.StatusCode)
	}
	var created struct {
		Name string `json:"name"`
	}
	if readErr != nil || response.StatusCode != http.StatusCreated || json.Unmarshal(data, &created) != nil || created.Name != member.Name {
		return errSpritesValidation
	}
	if err := s.requireSpriteMemberWanted(ctx, scope, member.Name); err != nil {
		return err
	}
	command := "detent hub runner register --url " + spriteShellQuote(s.config.Hosted.PublicURL) + " --organization " + spriteShellQuote(string(scope.Organization)) + " --name " + spriteShellQuote(member.Name) + " --capacity 1 --isolation-tier " + spriteShellQuote(settings.IsolationTier) + " --token " + spriteShellQuote(enrollment.Token)
	script := spritePoolBootstrapScript(scope.Organization, s.config.Hosted.PublicURL, settings.Bootstrap, command, version)
	log, execErr := s.execPoolSprite(ctx, scope, member.Name, []byte(script))
	if execErr != nil {
		log += "Bootstrap did not complete\n"
	}
	if err := s.updateSpritePoolMember(ctx, scope, member.Name, func(current *runnerstore.SpriteMembership) error {
		if current.EnrollmentID != member.EnrollmentID {
			return corestore.ErrRevisionConflict
		}
		current.BootstrapLog = log
		return nil
	}); err != nil {
		return errors.Join(execErr, err)
	}
	if execErr != nil {
		return execErr
	}
	if err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		enrollment, err := r.RunnerEnrollment(ctx, member.EnrollmentID)
		if err != nil {
			return err
		}
		if enrollment.RunnerID == "" || enrollment.RevokedAt != nil {
			return corestore.ErrNotFound
		}
		runner, err := r.RunnerIdentity(ctx, enrollment.RunnerID)
		if err != nil {
			return err
		}
		if runner.EnrollmentID != member.EnrollmentID || runner.RemovedAt != nil {
			return corestore.ErrNotFound
		}
		credential, err := r.AccessCredential(ctx, runner.TokenID)
		if err != nil {
			return err
		}
		now := s.config.now()
		if credential.RevokedAt != nil || credential.ExpiresAt != nil && !credential.ExpiresAt.After(now) {
			return corestore.ErrNotFound
		}
		machine, err := r.Machine(ctx, string(runner.MachineID))
		if err != nil {
			return err
		}
		if machine.SpriteName != member.Name || machine.Hostname != member.Name {
			return corestore.ErrNotFound
		}
		return nil
	}); err != nil {
		return err
	}
	if err := s.requireSpriteMemberWanted(ctx, scope, member.Name); err != nil {
		return err
	}
	response, err = s.poolSpriteRequest(ctx, scope, member.Name, http.MethodPost, "/"+member.Name+"/checkpoint", []byte(`{"comment":"Detent runner customer bootstrap"}`))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errSpritesValidation
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	complete := false
	for {
		var event struct {
			Type string `json:"type"`
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return errSpritesValidation
		}
		if event.Type == "error" {
			return errSpritesValidation
		}
		complete = complete || event.Type == "complete"
	}
	if !complete {
		return errSpritesValidation
	}
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		wanted, err := spriteMemberWantedRecord(ctx, w, member.Name)
		if err != nil {
			return err
		}
		if !wanted {
			return errSpriteReservationStale
		}
		owner, ok := w.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		row, err := owner.SpriteMember(ctx, member.Name)
		if err != nil {
			return err
		}
		if row.EnrollmentID != member.EnrollmentID {
			return corestore.ErrRevisionConflict
		}
		if row.State != "bootstrapping" {
			return errSpriteReservationStale
		}
		updated := spriteMembershipRecord(row)
		updated.State = "enrolled"
		updated.BootstrapLog += "Checkpoint completed\n"
		updated.IdleSince = s.config.now()
		return w.UpdateRunnerSpriteMembership(ctx, updated)
	})
}

func (s *Service) execPoolSprite(ctx context.Context, scope hubscope.NativeScope, name string, script []byte) (string, error) {
	defer clear(script)
	token, err := s.poolSpriteToken(ctx, scope, name)
	if err != nil {
		return "", err
	}
	defer clear(token)
	query := url.Values{"cmd": {"bash", "-s"}, "stdin": {"true"}, "max_run_after_disconnect": {"1s"}}
	header := http.Header{"Authorization": {"Bearer " + string(token)}}
	defer header.Del("Authorization")
	conn, response, err := websocket.Dial(ctx, "wss://api.sprites.dev/v1/sprites/"+name+"/exec?"+query.Encode(), &websocket.DialOptions{HTTPClient: s.spritePoolClient(), HTTPHeader: header})
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return "", errSpritesValidation
	}
	defer func() {
		if err := conn.CloseNow(); err != nil {
			s.config.Logger.Debug("close Sprite bootstrap connection", "error", err)
		}
	}()
	conn.SetReadLimit(1 << 20)
	input := append([]byte{0}, script...)
	defer clear(input)
	if err := conn.Write(ctx, websocket.MessageBinary, input); err != nil {
		return "", errSpritesValidation
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{4}); err != nil {
		return "", errSpritesValidation
	}
	var log strings.Builder
	pending := ""
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return log.String(), errSpritesValidation
		}
		if kind == websocket.MessageText {
			var event struct {
				Type     string `json:"type"`
				ExitCode *int   `json:"exit_code"`
			}
			if json.Unmarshal(data, &event) == nil {
				if event.Type == "error" {
					return log.String(), errSpritesValidation
				}
				if event.Type == "exit" && event.ExitCode != nil {
					if *event.ExitCode != 0 {
						fmt.Fprintf(&log, "Bootstrap exited with code %d\n", *event.ExitCode)
						return log.String(), fmt.Errorf("sprite bootstrap exited with code %d", *event.ExitCode)
					}
					return log.String(), nil
				}
			}
		}
		if kind == websocket.MessageBinary && len(data) > 0 {
			if data[0] == 3 && len(data) == 2 {
				if data[1] != 0 {
					fmt.Fprintf(&log, "Bootstrap exited with code %d\n", data[1])
					return log.String(), fmt.Errorf("sprite bootstrap exited with code %d", data[1])
				}
				return log.String(), nil
			}
			if data[0] == 1 && log.Len() < 8192 {
				pending += string(data[1:])
				for {
					line, rest, found := strings.Cut(pending, "\n")
					if !found {
						break
					}
					pending = rest
					if safe := spriteBootstrapDiagnostic(line); safe != "" && log.Len()+len(safe)+1 <= 8192 {
						log.WriteString(safe + "\n")
					}
				}
				if len(pending) > 256 {
					pending = ""
				}
			}
		}
	}
}

func (s *Service) deletePoolSprite(ctx context.Context, scope hubscope.NativeScope, member spritePoolMember) error {
	ready := false
	actor := ""
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		actor, err = owner.SpritePoolAuthority(ctx, s.config.now())
		if err != nil {
			return err
		}
		current, err := owner.SpriteMember(ctx, member.Name)
		if err != nil {
			return err
		}
		if current.EnrollmentID != member.EnrollmentID || current.AllocationProjectID != string(scope.Project) {
			return corestore.ErrRevisionConflict
		}
		if current.State == "deleted" {
			return nil
		}
		enrollment, err := w.RunnerEnrollment(ctx, current.EnrollmentID)
		if err != nil {
			return err
		}
		var identity runnerstore.Identity
		if enrollment.RunnerID != "" {
			identity, err = w.RunnerIdentity(ctx, enrollment.RunnerID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
		}
		now := s.config.now()
		if current.State == "enrolled" {
			pool, err := owner.SpritePool(ctx)
			if err != nil {
				return err
			}
			count, others, _, err := owner.SpriteMemberPlacementCounts(ctx, current.Name)
			if err != nil {
				return err
			}
			floor, threshold := int(pool.MinRunners), time.Duration(pool.IdleSeconds)*time.Second
			busy := false
			if identity.RunnerID != "" {
				usage, err := w.ClaimCapacityUsage(ctx, string(identity.MachineID), identity.RunnerID, scope.Project, "", now)
				if err != nil {
					return err
				}
				busy = usage.Host > 0
			}
			if scope.Project != "" {
				allocation, _, err := projectSpriteAllocationRecord(ctx, w, scope)
				if err != nil {
					return err
				}
				floor, threshold, busy = allocation.DesiredCount, 0, false
			}
			floor = min(floor, max(int(pool.MaxRunners)-others, 0))
			if busy || count <= floor || now.Sub(current.IdleSince) < threshold {
				return nil
			}
		}
		if identity.RunnerID != "" && identity.State == "active" {
			identity.State = "draining"
			if err := w.UpdateRunnerRouting(ctx, identity.Revision, identity); err != nil {
				return err
			}
		}
		if enrollment.RevokedAt == nil && enrollment.RedeemedAt == nil {
			if err := w.RevokeRunnerEnrollment(ctx, enrollment.ID, now); err != nil {
				return err
			}
		}
		replacement := spriteMembershipRecord(current)
		replacement.State = "deleting"
		if err := w.UpdateRunnerSpriteMembership(ctx, replacement); err != nil {
			return err
		}
		held, err := owner.SpriteMemberHoldsWork(ctx, current.EnrollmentID)
		if err != nil {
			return err
		}
		ready = !held
		return nil
	})
	if err != nil || !ready {
		return err
	}
	response, err := s.poolSpriteRequest(ctx, scope, member.Name, http.MethodDelete, "/"+member.Name, nil)
	if err != nil {
		return err
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusNotFound && (response.StatusCode < 200 || response.StatusCode > 299) {
		return errSpritesValidation
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.ReadTimeout)
	defer cancel()
	return s.config.TenantRepository.WriteTenant(cleanup, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(cleanup); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		currentActor, err := owner.SpritePoolAuthority(cleanup, s.config.now())
		if err != nil {
			return err
		}
		if currentActor != actor {
			return corestore.ErrRevisionConflict
		}
		current, err := owner.SpriteMember(cleanup, member.Name)
		if err != nil {
			return err
		}
		if current.EnrollmentID != member.EnrollmentID || current.AllocationProjectID != string(scope.Project) {
			return corestore.ErrRevisionConflict
		}
		held, err := owner.SpriteMemberHoldsWork(cleanup, current.EnrollmentID)
		if err != nil {
			return err
		}
		if held {
			return tracker.ErrLeaseConflict
		}
		enrollment, err := w.RunnerEnrollment(cleanup, current.EnrollmentID)
		if err != nil {
			return err
		}
		if enrollment.RunnerID != "" {
			if err := s.revokeRunnerIdentityTenant(cleanup, w, scope.Organization, enrollment.RunnerID, actor, s.config.now()); err != nil {
				return err
			}
		}
		replacement := spriteMembershipRecord(current)
		replacement.State = "deleted"
		return w.UpdateRunnerSpriteMembership(cleanup, replacement)
	})
}

func spriteMembershipRecord(row postgres.SpriteMember) runnerstore.SpriteMembership {
	return runnerstore.SpriteMembership{OrganizationID: row.OrganizationID, Name: row.Name, ProviderOrganization: row.ProviderOrganization, TokenProjectID: tracker.ProjectID(row.TokenProjectID), EnrollmentID: row.EnrollmentID, State: row.State, BootstrapLog: row.BootstrapLog, IdleSince: row.IdleSince, CreatedAt: row.CreatedAt, AllocationProjectID: tracker.ProjectID(row.AllocationProjectID), AllocationRevision: row.AllocationRevision}
}

func spriteMonthlyDecisionRecord(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, admitted bool, now time.Time) (budget.MonthlyDecision, error) {
	input, err := readBillingMonthlyBudgetInput(ctx, r, scope, now)
	if err != nil {
		return budget.MonthlyDecision{}, err
	}
	input.Exposure = budget.CostExposure{SpriteInfrastructure: true}
	input.Admitted = admitted
	return billingstore.EvaluateMonthlyBudget(input)
}

func spriteMonthlyAllowedRecord(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, now time.Time) (bool, error) {
	decision, err := spriteMonthlyDecisionRecord(ctx, r, scope, false, now)
	if err != nil || decision.Allowed {
		return decision.Allowed, err
	}
	decision, err = spriteMonthlyDecisionRecord(ctx, r, scope, true, now)
	if err != nil || !decision.Allowed {
		return false, err
	}
	return r.BillingMonthlyAdmittedCandidate(ctx, scope.Project, now)
}
