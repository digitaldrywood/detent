package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const spriteBootstrapSource = "https://raw.githubusercontent.com/digitaldrywood/detent/%s/scripts/sprite-runner-bootstrap.sh"

func spriteShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (s *Service) poolSpriteToken(ctx context.Context, scope nativeScope, name string) ([]byte, error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	actor, err := s.spritePoolAuthority(readCtx, s.database.reader, scope)
	if err != nil {
		return nil, err
	}
	scope.credential.ID = actor
	var project tracker.ProjectID
	var provider string
	err = s.database.reader.QueryRowContext(readCtx, `SELECT token_project_id,provider_organization FROM organization_sprite_members WHERE organization_id=? AND name=? AND state<>'deleted'`, scope.organization, name).Scan(&project, &provider)
	if err != nil {
		return nil, err
	}
	scope.project = project
	scope, slug, envelope, err := resolveSpritesSecret(readCtx, s.database.reader, scope)
	if err != nil {
		return nil, err
	}
	if slug != provider {
		return nil, errSpritesValidation
	}
	if err := s.auditSecretUse(ctx, scope, envelope.Version); err != nil {
		return nil, err
	}
	return s.config.SecretKeys.Open(envelope, secretAAD(string(scope.organization), string(scope.project), flySpritesToken))
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

func (s *Service) poolSpriteRequest(ctx context.Context, scope nativeScope, name, method, path string, body []byte) (*http.Response, error) {
	if method != http.MethodDelete && method != http.MethodGet {
		allowed, err := monthlySpriteAllowed(ctx, s.database.reader, scope, s.config.now())
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

func (s *Service) createPoolSprite(ctx context.Context, scope nativeScope, settings spritePoolSettings, allowOrganization bool) (bool, error) {
	scope.project = ""
	if s.config.Hosted == nil || settings.MaxRunners == 0 {
		return false, nil
	}
	if _, err := s.poolSpriteTokenAvailable(ctx, scope); err != nil {
		return false, err
	}
	name := "detent-" + strings.TrimPrefix(newNativeID("sprite"), "sprite_")
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	actor, err := s.spritePoolAuthority(ctx, tx, scope)
	if err != nil {
		return false, err
	}
	scope.credential = apiCredential{ID: actor}
	var ceiling, count int
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT max_runners,revision,(SELECT count(*) FROM organization_sprite_members m WHERE m.organization_id=p.organization_id AND m.state<>'deleted') FROM organization_sprite_pools p WHERE organization_id=?`, scope.organization).Scan(&ceiling, &revision, &count); err != nil {
		return false, err
	}
	if ceiling <= count || revision != settings.Revision {
		return false, nil
	}
	now := s.config.now()
	allowed, err := monthlySpriteAllowed(ctx, tx, scope, now)
	if err != nil || !allowed {
		return false, err
	}
	placement, err := readOrganizationPlacementSnapshot(ctx, tx, scope, now)
	if err != nil {
		return false, err
	}
	projects, err := readSpriteAllocationProjects(ctx, tx, scope)
	if err != nil {
		return false, err
	}
	var allocationRevision tracker.Revision
	for _, project := range projects {
		allocationScope := scope
		allocationScope.project = project
		allocation, err := readProjectSpriteAllocation(ctx, tx, allocationScope)
		if err != nil {
			return false, err
		}
		if allocation.Shortfall > 0 && allocation.ProvisionFailures < spriteProvisionAttempts {
			valid, err := s.projectSpriteAllocationAuthority(ctx, tx, allocationScope)
			if err != nil {
				return false, err
			}
			if !valid {
				continue
			}
			decision, err := checkMonthlyBudget(ctx, tx, allocationScope, budget.CostExposure{SpriteInfrastructure: true}, false, now)
			if err != nil {
				return false, err
			}
			if !decision.Allowed {
				continue
			}
			scope.project = project
			settings.MinRunners = allocation.DesiredCount
			allocationRevision = allocation.Revision
			break
		}
	}
	if scope.project == "" && !allowOrganization {
		return false, nil
	}
	ownerCount := 0
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM organization_sprite_members WHERE organization_id=? AND allocation_project_id=? AND state<>'deleted'`, scope.organization, scope.project).Scan(&ownerCount); err != nil {
		return false, err
	}
	input := spriteScaleInput{Depth: placement.SpriteTarget, Free: placement.SpriteFree, Pending: placement.Pending, Floor: settings.MinRunners, Ceiling: ceiling, Count: ownerCount, CreateLimit: placement.ProviderFree}
	decision, err := checkMonthlyBudget(ctx, tx, scope, budget.CostExposure{SpriteInfrastructure: true}, false, now)
	if err != nil {
		return false, err
	}
	if !decision.Allowed {
		input.CreateLimit = new(placement.Provisionable)
	}
	providerFree, providerKnown, err := spritePoolProviderFree(ctx, tx, scope, count, now)
	if err != nil {
		return false, err
	}
	if scope.project != "" {
		input.CreateLimit = nil
		input.Depth, input.Free, input.Pending = 0, 0, 0
	} else if placement.Policy.Mode == "local_first" {
		input.Floor = min(input.Floor, placement.Policy.OverflowSlots)
	}
	if providerKnown && (input.CreateLimit == nil || providerFree < *input.CreateLimit) {
		input.CreateLimit = new(providerFree)
	}
	if !decision.Allowed && scope.project != "" {
		input.CreateLimit = new(0)
	}
	input.Ceiling = min(input.Ceiling, ownerCount+ceiling-count)
	if decideSpriteScale(input).Create == 0 {
		return false, nil
	}
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return false, err
	}
	request := runnerauth.EnrollmentRequest{Scope: "organization", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: int64(runnerauth.MaxEnrollmentTTL / time.Second)}
	if scope.project != "" {
		request.Scope, request.ProjectIDs = "projects", []tracker.ProjectID{scope.project}
	}
	enrollment, err := s.createRunnerEnrollmentInTx(ctx, tx, scope, request, now)
	if err != nil {
		return false, err
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO organization_sprite_members(organization_id,name,provider_organization,enrollment_id,state,idle_since,created_at,allocation_project_id,allocation_revision) SELECT ?,?,organization_slug,?,'bootstrapping',?,?,?,? FROM organization_secrets WHERE organization_id=? AND kind=?`, scope.organization, name, enrollment.ID, formatHubTime(now), formatHubTime(now), scope.project, allocationRevision, scope.organization, flySpritesToken)
	if err := requireRunnerUpdate(inserted, err); err != nil {
		return false, err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	member := spritePoolMember{AllocationProjectID: scope.project, Name: name, State: "bootstrapping", EnrollmentID: enrollment.ID}
	if err := s.requireSpriteMemberWanted(ctx, scope, name); err != nil {
		return true, s.cancelPoolSpriteReservation(ctx, scope, member, err)
	}
	err = s.bootstrapPoolSprite(ctx, scope, member, settings, enrollment)
	if err == nil {
		return true, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
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

func (s *Service) requireSpriteMemberWanted(ctx context.Context, scope nativeScope, name string) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	wanted, err := spriteMemberWanted(ctx, s.database.reader, scope.organization, name)
	if err == nil && !wanted {
		err = errSpriteReservationStale
	}
	return err
}

func (s *Service) cancelPoolSpriteReservation(ctx context.Context, scope nativeScope, member spritePoolMember, cause error) error {
	if !errors.Is(cause, errSpriteReservationStale) {
		return errors.Join(cause, s.retirePoolSpriteReservation(ctx, scope, member.Name, "Sprite provisioning did not start\n", false))
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE runner_enrollments SET revoked_at=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`, formatHubTime(s.config.now()), member.EnrollmentID, scope.organization); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE organization_sprite_members SET state='deleted',bootstrap_log=bootstrap_log || ? WHERE organization_id=? AND name=? AND state='bootstrapping'`, errSpriteReservationStale.Error()+" before paid creation; its reservation was cancelled\n", scope.organization, member.Name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) retirePoolSpriteReservation(ctx context.Context, scope nativeScope, name, log string, failed bool) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE organization_sprite_members SET state='deleting',bootstrap_log=bootstrap_log || ? WHERE organization_id=? AND name=? AND state IN ('bootstrapping','enrolled')`, log, scope.organization, name)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 && failed {
		if _, err := tx.ExecContext(ctx, `UPDATE project_sprite_allocations SET provision_failures=provision_failures+1 WHERE organization_id=? AND EXISTS(SELECT 1 FROM organization_sprite_members m WHERE m.organization_id=project_sprite_allocations.organization_id AND m.name=? AND m.allocation_project_id=project_sprite_allocations.project_id AND m.allocation_revision=project_sprite_allocations.revision)`, scope.organization, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) poolSpriteTokenAvailable(ctx context.Context, scope nativeScope) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var count int
	err := s.database.reader.QueryRowContext(ctx, `SELECT count(*) FROM organization_secrets WHERE organization_id=? AND kind=?`, scope.organization, flySpritesToken).Scan(&count)
	if err == nil && count == 0 {
		err = sql.ErrNoRows
	}
	return count != 0, err
}

func (s *Service) bootstrapPoolSprite(ctx context.Context, scope nativeScope, member spritePoolMember, settings spritePoolSettings, enrollment runnerauth.Enrollment) error {
	version := "v" + strings.TrimPrefix(s.config.Version, "v")
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return nativePublicInvalid("Sprite provisioning requires a Hub built from a pinned release")
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
	command := "detent hub runner register --url " + spriteShellQuote(s.config.Hosted.PublicURL) + " --organization " + spriteShellQuote(string(scope.organization)) + " --name " + spriteShellQuote(member.Name) + " --capacity 1 --isolation-tier " + spriteShellQuote(settings.IsolationTier) + " --token " + spriteShellQuote(enrollment.Token)
	script := fmt.Sprintf(`set -euo pipefail
set +x
export DETENT_ORGANIZATION_ID=%s
export DETENT_HUB_URL=%s
export TMPDIR="$HOME/detent-runner/.tmp"
mkdir -p -- "$TMPDIR"
printf "Customer setup started\n"
(
%s
) >/dev/null 2>&1
printf "Customer setup completed\nRunner bootstrap started\n"
bootstrap=$(mktemp "$TMPDIR/sprite-bootstrap.XXXXXX")
trap 'rm -f -- "$bootstrap"' EXIT
curl --fail --silent --show-error --location %s --output "$bootstrap" >/dev/null 2>&1
printf '%%s\n' %s | bash "$bootstrap" --version %s >/dev/null 2>&1
printf "Runner bootstrap completed\n"
`, spriteShellQuote(string(scope.organization)), spriteShellQuote(s.config.Hosted.PublicURL), settings.Bootstrap, spriteShellQuote(fmt.Sprintf(spriteBootstrapSource, version)), spriteShellQuote(command), spriteShellQuote(version))
	log, execErr := s.execPoolSprite(ctx, scope, member.Name, []byte(script))
	if execErr != nil {
		log += "Bootstrap did not complete\n"
	}
	if _, err := s.database.db.ExecContext(ctx, `UPDATE organization_sprite_members SET bootstrap_log=? WHERE organization_id=? AND name=?`, log, scope.organization, member.Name); err != nil {
		return errors.Join(execErr, err)
	}
	if execErr != nil {
		return execErr
	}
	var runner string
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if err := s.database.reader.QueryRowContext(readCtx, `SELECT r.id FROM runner_identities r JOIN machines m ON m.id=r.machine_id JOIN api_tokens t ON t.id=r.token_id AND t.revoked_at IS NULL WHERE r.enrollment_id=? AND r.organization_id=? AND m.hostname=? AND json_extract(m.capabilities_json,'$.sprite_name')=?`, member.EnrollmentID, scope.organization, member.Name, member.Name).Scan(&runner); err != nil {
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
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	wanted, err := spriteMemberWanted(ctx, tx, scope.organization, member.Name)
	if err != nil {
		return err
	}
	if !wanted {
		return errSpriteReservationStale
	}
	if _, err := tx.ExecContext(ctx, `UPDATE organization_sprite_members SET state='enrolled',bootstrap_log=bootstrap_log || ?,idle_since=? WHERE organization_id=? AND name=? AND state='bootstrapping'`, "Checkpoint completed\n", formatHubTime(s.config.now()), scope.organization, member.Name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) execPoolSprite(ctx context.Context, scope nativeScope, name string, script []byte) (string, error) {
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
					switch line {
					case "Customer setup started", "Customer setup completed", "Runner bootstrap started", "Runner bootstrap completed":
						log.WriteString(line + "\n")
					}
				}
				if len(pending) > 256 {
					pending = ""
				}
			}
		}
	}
}

func (s *Service) deletePoolSprite(ctx context.Context, scope nativeScope, member spritePoolMember) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actor, err := s.spritePoolAuthority(ctx, tx, scope)
	if err != nil {
		return err
	}
	scope.credential.ID = actor
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM organization_sprite_members WHERE organization_id=? AND name=? AND enrollment_id=? AND allocation_project_id=?`, scope.organization, member.Name, member.EnrollmentID, scope.project).Scan(&state); err != nil {
		return err
	}
	if state == "deleted" {
		return nil
	}
	if state == "enrolled" {
		var count, floor, ceiling, otherMembers int
		var idle string
		var threshold int
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT p.min_runners,p.max_runners,p.idle_seconds,m.idle_since,(SELECT count(*) FROM organization_sprite_members a WHERE a.organization_id=p.organization_id AND a.allocation_project_id=m.allocation_project_id AND a.state NOT IN ('deleted','deleting')),(SELECT count(*) FROM organization_sprite_members a WHERE a.organization_id=p.organization_id AND a.allocation_project_id<>m.allocation_project_id AND a.state NOT IN ('deleted','deleting')),EXISTS(SELECT 1 FROM leases l JOIN runner_identities r ON r.machine_id=l.machine_id WHERE r.enrollment_id=m.enrollment_id AND r.organization_id=m.organization_id AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)) FROM organization_sprite_pools p JOIN organization_sprite_members m ON m.organization_id=p.organization_id WHERE p.organization_id=? AND m.name=?`, formatHubTime(s.config.now()), scope.organization, member.Name).Scan(&floor, &ceiling, &threshold, &idle, &count, &otherMembers, &busy); err != nil {
			return err
		}
		if scope.project != "" {
			allocation, err := readProjectSpriteAllocation(ctx, tx, scope)
			if err != nil {
				return err
			}
			floor, threshold, busy = allocation.DesiredCount, 0, false
		}
		floor = min(floor, max(ceiling-otherMembers, 0))
		at, err := parseTimeValue(idle)
		if err != nil {
			return err
		}
		if busy || count <= floor || s.config.now().Sub(at) < time.Duration(threshold)*time.Second {
			return nil
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_identities SET state='draining',revision=revision+1 WHERE enrollment_id=? AND organization_id=? AND state='active'`, member.EnrollmentID, scope.organization); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_enrollments SET revoked_at=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`, formatHubTime(s.config.now()), member.EnrollmentID, scope.organization); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE organization_sprite_members SET state='deleting' WHERE organization_id=? AND name=? AND state<>'deleted'`, scope.organization, member.Name); err != nil {
		return err
	}
	unrecovered, err := spriteMemberHoldsWork(ctx, tx, scope.organization, member.EnrollmentID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if unrecovered {
		return nil
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
	finished, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finished.Rollback()
	if _, err := finished.ExecContext(ctx, `UPDATE organization_sprite_members SET state='deleted' WHERE organization_id=? AND name=?`, scope.organization, member.Name); err != nil {
		return err
	}
	var runnerID string
	err = finished.QueryRowContext(ctx, `SELECT r.id FROM runner_identities r JOIN api_tokens t ON t.id=r.token_id WHERE r.enrollment_id=? AND r.organization_id=? AND t.revoked_at IS NULL`, member.EnrollmentID, scope.organization).Scan(&runnerID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if err := revokeRunnerIdentityInTx(ctx, finished, scope, runnerID, s.config.now()); err != nil {
			return err
		}
	}
	return finished.Commit()
}

func spriteMemberHoldsWork(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, enrollment string) (bool, error) {
	var held bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM leases l JOIN runner_identities r ON r.machine_id=l.machine_id WHERE r.enrollment_id=? AND r.organization_id=? AND l.released_at IS NULL)`, enrollment, organization).Scan(&held)
	return held, err
}
