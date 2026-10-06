package cloudentry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type PlatformConfig struct {
	BootstrapAdminEmail string `yaml:"bootstrap_admin_email"`
}

const (
	platformMemberList   = "platform_members_list"
	platformMemberAdd    = "platform_members_add"
	platformMemberChange = "platform_members_change"
	platformMemberRemove = "platform_members_remove"
)

type platformMember struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	AddedBy   string `json:"added_by"`
	AddedAt   string `json:"added_at"`
	UpdatedAt string `json:"updated_at"`
}

type platformMembersResult struct {
	Members  []platformMember `json:"members"`
	Revision int64            `json:"revision"`
	Self     struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	} `json:"self"`
}

func platformEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func validPlatformEmail(email string) bool {
	address, err := mail.ParseAddress(email)
	return err == nil && address.Address == email && len(email) <= 254
}

func (s *Service) platformRole(ctx context.Context, email string) string {
	var role string
	if err := s.registry.store.db.QueryRowContext(ctx, "SELECT role FROM platform_members WHERE email=?", platformEmail(email)).Scan(&role); err != nil {
		return ""
	}
	return role
}

func (s *Service) sessionPlatformRole(ctx context.Context, session accountSession) string {
	if session.Identity.SupportActor != "" {
		return ""
	}
	return s.platformRole(ctx, session.Email)
}

func (r *Registry) seedPlatformMembers(ctx context.Context, cfg Config) error {
	bootstrap := platformEmail(cfg.Platform.BootstrapAdminEmail)
	if bootstrap != "" && !validPlatformEmail(bootstrap) {
		return errors.New("platform.bootstrap_admin_email must be an email address")
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM platform_members").Scan(&count); err != nil {
		return err
	}
	roles := map[string]string{}
	if count == 0 {
		for _, email := range cfg.StaffEmails {
			roles[platformEmail(email)] = "viewer"
		}
		for _, email := range cfg.SupportActors {
			roles[platformEmail(email)] = "support"
		}
		for _, email := range cfg.EntitlementAdministrators {
			email = platformEmail(email)
			if roles[email] == "support" || roles[email] == "admin" {
				roles[email] = "admin"
			} else {
				roles[email] = "billing"
			}
		}
	}
	if bootstrap != "" {
		roles[bootstrap] = "admin"
	}
	emails := make([]string, 0, len(roles))
	for email := range roles {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	now := formatTime(cfg.now())
	for _, email := range emails {
		if !validPlatformEmail(email) {
			return fmt.Errorf("invalid platform seed email %q", email)
		}
		var previous string
		err := tx.QueryRowContext(ctx, "SELECT role FROM platform_members WHERE email=?", email).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if previous == roles[email] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO platform_members(email,role,added_by,added_at,updated_at) VALUES(?,?,'bootstrap',?,?) ON CONFLICT(email) DO UPDATE SET role=excluded.role,updated_at=excluded.updated_at`, email, roles[email], now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO platform_member_changes(email,action,previous_role,role,actor_email,actor_subject,reason,recorded_at) VALUES(?,'seeded',?,?,'bootstrap','','Platform startup bootstrap',?)`, email, previous, roles[email], now); err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM platform_members WHERE role='admin'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return errors.New("platform requires platform.bootstrap_admin_email or an existing admin member")
	}
	return tx.Commit()
}

func platformMemberOperation(name string) bool {
	return name == platformMemberList || name == platformMemberAdd || name == platformMemberChange || name == platformMemberRemove
}

func (s *Service) readPlatformMembers(ctx context.Context, session accountSession) (platformMembersResult, error) {
	result := platformMembersResult{Members: []platformMember{}}
	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT email,role,added_by,added_at,updated_at FROM platform_members ORDER BY email")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var member platformMember
		if err := rows.Scan(&member.Email, &member.Role, &member.AddedBy, &member.AddedAt, &member.UpdatedAt); err != nil {
			return result, err
		}
		result.Members = append(result.Members, member)
		if member.Email == platformEmail(session.Email) {
			result.Self.Email, result.Self.Role = member.Email, member.Role
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return result, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM platform_member_changes").Scan(&result.Revision); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (a entryAdministration) executePlatformMember(ctx context.Context, name string, in operatoradmin.Input, m mutation.Metadata, session accountSession) (operatoradmin.Output, error) {
	s := a.service
	if name != platformMemberAdd && name != platformMemberChange && name != platformMemberRemove || in.Email != platformEmail(in.Email) || !validPlatformEmail(in.Email) || strings.TrimSpace(in.Reason) == "" || utf8.RuneCountInString(in.Reason) > 500 || in.ExpectedRevision < 0 || name != platformMemberRemove && (in.Role == "" || !cloudassert.ValidPlatformRole(in.Role)) || name == platformMemberRemove && in.Role != "" {
		return operatoradmin.Output{}, operatortool.ErrInvalidArguments
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if err := a.Authorize(ctx, name, in, ""); err != nil {
		return operatoradmin.Output{}, err
	}
	if out, found, err := s.administrationReceipt(ctx, session, m); err != nil || found {
		return out, err
	}
	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return operatoradmin.Output{}, err
	}
	defer tx.Rollback()
	var revision int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM platform_member_changes").Scan(&revision); err != nil {
		return operatoradmin.Output{}, err
	}
	if revision != in.ExpectedRevision {
		return operatoradmin.Output{}, mutation.ErrConflict
	}
	var previous string
	err = tx.QueryRowContext(ctx, "SELECT role FROM platform_members WHERE email=?", in.Email).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return operatoradmin.Output{}, err
	}
	if name == platformMemberAdd && previous != "" {
		return operatoradmin.Output{}, mutation.ErrConflict
	}
	if name != platformMemberAdd && previous == "" {
		return operatoradmin.Output{}, sql.ErrNoRows
	}
	if name == platformMemberChange && in.Email == platformEmail(session.Email) {
		return operatoradmin.Output{}, operatortool.ErrAccessDenied
	}
	if name == platformMemberRemove && in.Email == platformEmail(s.config.Platform.BootstrapAdminEmail) {
		return operatoradmin.Output{}, operatortool.ErrAccessDenied
	}
	if previous == "admin" && (name == platformMemberRemove || in.Role != "admin") {
		var admins int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM platform_members WHERE role='admin'").Scan(&admins); err != nil {
			return operatoradmin.Output{}, err
		}
		if admins <= 1 {
			return operatoradmin.Output{}, operatortool.ErrAccessDenied
		}
	}
	if err := s.recordAdministrationReceipt(ctx, session, m, "pending", operatoradmin.Output{}); err != nil {
		return operatoradmin.Output{}, err
	}
	now := formatTime(s.config.now())
	action, role := "added", in.Role
	switch name {
	case platformMemberAdd:
		_, err = tx.ExecContext(ctx, "INSERT INTO platform_members(email,role,added_by,added_at,updated_at) VALUES(?,?,?,?,?)", in.Email, in.Role, platformEmail(session.Email), now, now)
	case platformMemberChange:
		action = "role_changed"
		_, err = tx.ExecContext(ctx, "UPDATE platform_members SET role=?,updated_at=? WHERE email=?", in.Role, now, in.Email)
	case platformMemberRemove:
		action, role = "removed", ""
		_, err = tx.ExecContext(ctx, "DELETE FROM platform_members WHERE email=?", in.Email)
	}
	if err != nil {
		return operatoradmin.Output{}, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO platform_member_changes(email,action,previous_role,role,actor_email,actor_subject,reason,recorded_at) VALUES(?,?,?,?,?,?,?,?)", in.Email, action, previous, role, platformEmail(session.Email), session.Subject, in.Reason, now); err != nil {
		return operatoradmin.Output{}, err
	}
	if err := tx.Commit(); err != nil {
		return operatoradmin.Output{}, err
	}
	data, err := json.Marshal(map[string]any{"email": in.Email, "role": role, "revision": revision + 1})
	if err != nil {
		return operatoradmin.Output{}, err
	}
	out := operatoradmin.Output{ResourceID: in.Email, Data: data}
	if err := s.recordAdministrationReceipt(context.WithoutCancel(ctx), session, m, "succeeded", out); err != nil {
		return operatoradmin.Output{}, mutation.ErrUncertain
	}
	return out, nil
}

func (s *Service) platformMembersAuthority(c echo.Context, name string) (context.Context, accountSession, error) {
	ctx := c.Request().Context()
	session, err := s.session(c)
	subject := session.Subject
	if auditErr := s.auth.audit(ctx, subject, "", name); auditErr != nil {
		return ctx, session, operatoradmin.ErrUnavailable
	}
	if err != nil {
		return ctx, session, errNoSession
	}
	id := operatortool.Identity{PrincipalID: session.Subject, CredentialID: session.Hash, SessionID: session.Hash, OrganizationID: "account:" + session.Subject}
	ctx = operatortool.WithConnection(ctx, operatortool.Connection{Identity: id})
	return ctx, session, (entryAdministration{s}).Authorize(ctx, name, operatoradmin.Input{}, "")
}

func (s *Service) platformMemberFailure(c echo.Context, err error) error {
	status, code, message := http.StatusServiceUnavailable, "unavailable", "Platform membership is temporarily unavailable"
	switch {
	case errors.Is(err, errNoSession):
		status, code, message = http.StatusUnauthorized, "unauthenticated", "Sign in to continue"
	case errors.Is(err, operatortool.ErrAccessDenied):
		status, code, message = http.StatusForbidden, "forbidden", "This platform membership operation is not allowed"
	case errors.Is(err, operatortool.ErrInvalidArguments):
		status, code, message = http.StatusUnprocessableEntity, "invalid_request", "Supply an email, valid role, reason of 1–500 characters, idempotency key and expected revision"
	case errors.Is(err, mutation.ErrConflict):
		status, code, message = http.StatusConflict, "revision_conflict", "Someone changed the staff list; reload and try again"
	case errors.Is(err, sql.ErrNoRows):
		status, code, message = http.StatusNotFound, "not_found", "That platform member does not exist"
	case errors.Is(err, mutation.ErrUncertain):
		status, code, message = http.StatusConflict, "outcome_uncertain", "The membership change outcome requires verification"
	}
	return c.JSON(status, map[string]string{"code": code, "message": message})
}

func (s *Service) platformMembersJSON(c echo.Context) error {
	ctx, _, err := s.platformMembersAuthority(c, platformMemberList)
	if err != nil {
		return s.platformMemberFailure(c, err)
	}
	result, err := (entryAdministration{s}).Read(ctx, platformMemberList, operatoradmin.Input{})
	if err != nil {
		return s.platformMemberFailure(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) changePlatformMember(c echo.Context) error {
	name := platformMemberAdd
	switch c.Request().Method {
	case http.MethodPatch:
		name = platformMemberChange
	case http.MethodDelete:
		name = platformMemberRemove
	}
	ctx, session, err := s.platformMembersAuthority(c, name)
	if err != nil {
		return s.platformMemberFailure(c, err)
	}
	if !s.csrfValid(c, session, "") {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "csrf_invalid", "message": "Reload the page and try again"})
	}
	var change struct {
		Email            string `json:"email"`
		Role             string `json:"role"`
		Reason           string `json:"reason"`
		IdempotencyKey   string `json:"idempotency_key"`
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return s.platformMemberFailure(c, operatortool.ErrInvalidArguments)
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return s.platformMemberFailure(c, operatortool.ErrInvalidArguments)
	}
	if name != platformMemberAdd {
		if change.Email != "" {
			return s.platformMemberFailure(c, operatortool.ErrInvalidArguments)
		}
		change.Email = c.Param("email")
	}
	change.Email, change.Reason = platformEmail(change.Email), strings.TrimSpace(change.Reason)
	if change.ExpectedRevision == nil || *change.ExpectedRevision < 0 || !safeID(change.IdempotencyKey) || !validPlatformEmail(change.Email) || utf8.RuneCountInString(change.Reason) < 1 || utf8.RuneCountInString(change.Reason) > 500 || name != platformMemberRemove && (change.Role == "" || !cloudassert.ValidPlatformRole(change.Role)) || name == platformMemberRemove && change.Role != "" {
		return s.platformMemberFailure(c, operatortool.ErrInvalidArguments)
	}
	in := operatoradmin.Input{Email: change.Email, Role: change.Role, Reason: change.Reason, ExpectedRevision: *change.ExpectedRevision}
	raw, err := json.Marshal(struct {
		Name  string
		Input operatoradmin.Input
	}{name, in})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	m := mutation.Metadata{OrganizationID: "account:" + session.Subject, ResourceID: change.Email, RetryIdentity: change.IdempotencyKey, InputHash: hex.EncodeToString(digest[:])}
	out, err := (entryAdministration{s}).Execute(ctx, name, in, m)
	if err != nil {
		return s.platformMemberFailure(c, err)
	}
	return c.JSONBlob(http.StatusOK, out.Data)
}
