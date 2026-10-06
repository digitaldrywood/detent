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

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const spriteBootstrapSource = "https://raw.githubusercontent.com/digitaldrywood/detent/a914d42bd00ad7d022673e342eb638ad255079c1/scripts/sprite-runner-bootstrap.sh"

func spriteShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (s *Service) poolSpriteToken(ctx context.Context, scope nativeScope, name string) ([]byte, error) {
	actor, err := s.spritePoolAuthority(ctx, s.database.db, scope)
	if err != nil {
		return nil, err
	}
	scope.credential.ID = actor
	var envelope hubsecrets.Envelope
	err = s.database.db.QueryRowContext(ctx, `SELECT ps.ciphertext,ps.nonce,ps.wrapped_data_key,ps.master_key_version FROM project_secrets ps JOIN project_sprite_members m ON m.organization_id=ps.organization_id AND m.project_id=ps.project_id AND m.provider_organization=ps.organization_slug WHERE ps.organization_id=? AND ps.project_id=? AND ps.kind=? AND m.name=? AND m.state<>'deleted'`, scope.organization, scope.project, flySpritesToken, name).Scan(&envelope.Ciphertext, &envelope.Nonce, &envelope.WrappedKey, &envelope.Version)
	if err != nil {
		return nil, err
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

func (s *Service) createPoolSprite(ctx context.Context, scope nativeScope, settings spritePoolSettings) error {
	if s.config.Hosted == nil || settings.MaxRunners == 0 {
		return nil
	}
	if _, err := s.poolSpriteTokenAvailable(ctx, scope); err != nil {
		return err
	}
	name := "detent-" + strings.TrimPrefix(newNativeID("sprite"), "sprite_")
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actor, err := s.spritePoolAuthority(ctx, tx, scope)
	if err != nil {
		return err
	}
	scope.credential = apiCredential{ID: actor}
	var ceiling, count int
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT max_runners,revision,(SELECT count(*) FROM project_sprite_members m WHERE m.organization_id=p.organization_id AND m.project_id=p.project_id AND m.state<>'deleted') FROM project_sprite_pools p WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&ceiling, &revision, &count); err != nil {
		return err
	}
	if ceiling <= count || revision != settings.Revision {
		return nil
	}
	now := s.config.now()
	placement, err := readPlacementSnapshot(ctx, tx, scope, now, nil)
	if err != nil {
		return err
	}
	input := spriteScaleInput{Depth: placement.SpriteTarget, Free: placement.SpriteFree, Pending: placement.Pending, Floor: settings.MinRunners, Ceiling: ceiling, Count: count, CreateLimit: placement.ProviderFree}
	if placement.Policy.Mode == "local_first" {
		input.Floor = min(input.Floor, placement.Policy.OverflowSlots)
	}
	if decideSpriteScale(input).Create == 0 {
		return nil
	}
	before, err := s.database.hostedConsumption(ctx, tx, now)
	if err != nil {
		return err
	}
	enrollment, err := s.createRunnerEnrollmentInTx(ctx, tx, scope, runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{scope.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: int64(runnerauth.MaxEnrollmentTTL / time.Second)}, now)
	if err != nil {
		return err
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO project_sprite_members(organization_id,project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) SELECT ?,?,?,organization_slug,?,'bootstrapping',?,? FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, name, enrollment.ID, formatHubTime(now), formatHubTime(now), scope.organization, scope.project, flySpritesToken)
	if err := requireRunnerUpdate(inserted, err); err != nil {
		return err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	member := spritePoolMember{Name: name, State: "bootstrapping", EnrollmentID: enrollment.ID}
	err = s.bootstrapPoolSprite(ctx, scope, member, settings, enrollment)
	if err == nil {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	failure := "Sprite provisioning did not complete; check customer setup and retry\n"
	if errors.Is(err, errSpritesTokenRejected) {
		failure = errSpritesTokenRejected.Error() + "\n"
	} else if errors.Is(err, errSpritesBilling) {
		failure = errSpritesBilling.Error() + "\n"
	}
	_, markErr := s.database.db.ExecContext(cleanup, `UPDATE project_sprite_members SET state='deleting',bootstrap_log=bootstrap_log || ? WHERE organization_id=? AND project_id=? AND name=?`, failure, scope.organization, scope.project, name)
	if ctx.Err() != nil {
		return errors.Join(err, markErr)
	}
	return errors.Join(err, markErr, s.deletePoolSprite(ctx, scope, member))
}

func (s *Service) poolSpriteTokenAvailable(ctx context.Context, scope nativeScope) (bool, error) {
	var count int
	err := s.database.db.QueryRowContext(ctx, `SELECT count(*) FROM project_secrets WHERE organization_id=? AND project_id=? AND kind=?`, scope.organization, scope.project, flySpritesToken).Scan(&count)
	if err == nil && count == 0 {
		err = sql.ErrNoRows
	}
	return count != 0, err
}

func (s *Service) bootstrapPoolSprite(ctx context.Context, scope nativeScope, member spritePoolMember, settings spritePoolSettings, enrollment runnerauth.Enrollment) error {
	version := "v" + strings.TrimPrefix(s.config.Version, "v")
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		return nativeInvalid("Sprite provisioning requires a Hub built from a pinned release")
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
	command := "detent hub runner register --url " + spriteShellQuote(s.config.Hosted.PublicURL) + " --organization " + spriteShellQuote(string(scope.organization)) + " --name " + spriteShellQuote(member.Name) + " --capacity 1 --token " + spriteShellQuote(enrollment.Token)
	script := fmt.Sprintf(`set -euo pipefail
set +x
export DETENT_PROJECT_ID=%s
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
`, spriteShellQuote(string(scope.project)), spriteShellQuote(s.config.Hosted.PublicURL), settings.Bootstrap, spriteShellQuote(spriteBootstrapSource), spriteShellQuote(command), spriteShellQuote(version))
	log, execErr := s.execPoolSprite(ctx, scope, member.Name, []byte(script))
	if execErr != nil {
		log += "Bootstrap did not complete\n"
	}
	if _, err := s.database.db.ExecContext(ctx, `UPDATE project_sprite_members SET bootstrap_log=? WHERE organization_id=? AND project_id=? AND name=?`, log, scope.organization, scope.project, member.Name); err != nil {
		return errors.Join(execErr, err)
	}
	if execErr != nil {
		return execErr
	}
	var runner string
	if err := s.database.db.QueryRowContext(ctx, `SELECT r.id FROM runner_identities r JOIN machines m ON m.id=r.machine_id JOIN api_tokens t ON t.id=r.token_id AND t.revoked_at IS NULL JOIN token_grants g ON g.token_id=r.token_id AND g.organization_id=r.organization_id AND g.project_id=? WHERE r.enrollment_id=? AND r.organization_id=? AND m.hostname=? AND json_extract(m.capabilities_json,'$.sprite_name')=?`, scope.project, member.EnrollmentID, scope.organization, member.Name, member.Name).Scan(&runner); err != nil {
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
	_, err = s.database.db.ExecContext(ctx, `UPDATE project_sprite_members SET state='enrolled',bootstrap_log=bootstrap_log || ?,idle_since=? WHERE organization_id=? AND project_id=? AND name=?`, "Checkpoint completed\n", formatHubTime(s.config.now()), scope.organization, scope.project, member.Name)
	return err
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
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM leases l JOIN runner_identities r ON r.machine_id=l.machine_id WHERE r.enrollment_id=? AND r.organization_id=? AND l.released_at IS NULL AND julianday(l.expires_at)>julianday(?)`, member.EnrollmentID, scope.organization, formatHubTime(s.config.now())).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return nil
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM project_sprite_members WHERE organization_id=? AND project_id=? AND name=? AND enrollment_id=?`, scope.organization, scope.project, member.Name, member.EnrollmentID).Scan(&state); err != nil {
		return err
	}
	if state == "deleted" {
		return nil
	}
	if state == "enrolled" {
		var count, floor int
		var idle string
		var threshold int
		if err := tx.QueryRowContext(ctx, `SELECT p.min_runners,p.idle_seconds,m.idle_since,(SELECT count(*) FROM project_sprite_members a WHERE a.organization_id=p.organization_id AND a.project_id=p.project_id AND a.state<>'deleted') FROM project_sprite_pools p JOIN project_sprite_members m ON m.organization_id=p.organization_id AND m.project_id=p.project_id WHERE p.organization_id=? AND p.project_id=? AND m.name=?`, scope.organization, scope.project, member.Name).Scan(&floor, &threshold, &idle, &count); err != nil {
			return err
		}
		at, err := parseTimeValue(idle)
		if err != nil {
			return err
		}
		if count <= floor || s.config.now().Sub(at) < time.Duration(threshold)*time.Second {
			return nil
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_identities SET state='draining',revision=revision+1 WHERE enrollment_id=? AND organization_id=? AND state='active'`, member.EnrollmentID, scope.organization); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_enrollments SET revoked_at=? WHERE id=? AND organization_id=? AND revoked_at IS NULL`, formatHubTime(s.config.now()), member.EnrollmentID, scope.organization); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_sprite_members SET state='deleting' WHERE organization_id=? AND project_id=? AND name=? AND state<>'deleted'`, scope.organization, scope.project, member.Name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
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
	finished, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finished.Rollback()
	if _, err := finished.ExecContext(ctx, `UPDATE project_sprite_members SET state='deleted' WHERE organization_id=? AND project_id=? AND name=?`, scope.organization, scope.project, member.Name); err != nil {
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
