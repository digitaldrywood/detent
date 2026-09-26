package cloudentry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type AllocationConfig struct {
	TenantRoot              string
	SocketRoot              string
	MaxTenants              int
	MaxConcurrent           int
	MaxPerIdentity          int
	RetryLimit              int
	MinFreeDiskBytes        uint64
	MinAvailableMemoryBytes uint64
	AllowedEmails           []string
	AllowedDomains          []string
	Launcher                Launcher
}

func (a *AllocationConfig) validate() error {
	if a == nil {
		return nil
	}
	if !filepath.IsAbs(a.TenantRoot) || !filepath.IsAbs(a.SocketRoot) || a.Launcher == nil {
		return errors.New("allocation requires absolute tenant and socket roots and a launcher")
	}
	if a.MaxTenants < 1 || a.MaxConcurrent < 1 || a.MaxPerIdentity < 1 || a.RetryLimit < 1 {
		return errors.New("allocation limits must be positive")
	}
	return nil
}

var provisioningSteps = []string{"admission", "provider_organization", "owner_membership", "tenant_files", "tenant_start", "owner_bootstrap", "publish"}

var errCapacity = errors.New("no capacity")

type terminalError struct{ code string }

func (e terminalError) Error() string { return e.code }

func nextStep(completed string) string {
	for i, step := range provisioningSteps {
		if step == completed && i+1 < len(provisioningSteps) {
			return provisioningSteps[i+1]
		}
	}
	return provisioningSteps[0]
}

func (s *Service) startAllocator(parent context.Context) {
	allocation := s.config.Allocation
	if allocation == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s.stopAllocator = cancel
	s.allocatorDone = make(chan struct{})
	s.wake = make(chan struct{}, 1)
	organizations, err := s.registry.List(ctx)
	if err == nil {
		for _, organization := range organizations {
			if organization.Managed && organization.State == "ready" {
				if err := allocation.Launcher.Start(ctx, s.tenantSpec(organization)); err != nil {
					s.config.Logger.Warn("tenant Hub could not start", "organization", organization.ID)
				}
			}
		}
	}
	go func() {
		defer close(s.allocatorDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			s.resumeDeletions(ctx)
			s.provisionDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-s.wake:
			}
		}
	}()
}

func (s *Service) closeAllocator() error {
	if s.stopAllocator == nil {
		return nil
	}
	s.stopAllocator()
	<-s.allocatorDone
	return s.config.Allocation.Launcher.Close()
}

func (s *Service) wakeAllocator() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) tenantSpec(organization Organization) TenantSpec {
	return TenantSpec{
		Organization: organization, Directory: filepath.Join(s.config.Allocation.TenantRoot, organization.ID),
		Socket: strings.TrimPrefix(organization.Endpoint, "unix:"), PublicURL: s.config.PublicURL, Issuer: s.config.Issuer,
		PublicKey: cloudassert.PublicKeyOf(s.config.SigningKey),
	}
}

func (s *Service) provisionDue(ctx context.Context) {
	due, err := s.dueOrganizations(ctx)
	if err != nil {
		return
	}
	now := s.config.now()
	for _, organization := range due {
		if organization.NextAttemptAt != "" {
			if next, err := parseTime(organization.NextAttemptAt); err == nil && next.After(now) {
				continue
			}
		}
		s.provision(ctx, organization)
	}
}

func (s *Service) dueOrganizations(ctx context.Context) ([]Organization, error) {
	rows, err := s.registry.store.db.QueryContext(ctx, organizationSelect+" WHERE managed = 1 AND state IN ('requested','allocating') ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var due []Organization
	for rows.Next() {
		organization, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, organization)
	}
	return due, rows.Err()
}

func (s *Service) provision(ctx context.Context, organization Organization) {
	for organization.State == "requested" || organization.State == "allocating" {
		step := nextStep(organization.Step)
		err := s.runStep(ctx, &organization, step)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.recordFailure(ctx, organization, step, err)
			return
		}
		organization.Step, organization.Attempts = step, 0
		if step == "publish" {
			organization.State = "ready"
		}
		if _, err := s.registry.store.db.ExecContext(ctx, "UPDATE organizations SET state = ?, provider_id = ?, step = ?, attempts = 0, next_attempt_at = '', error_code = '', updated_at = ? WHERE id = ?",
			organization.State, organization.ProviderID, step, formatTime(s.config.now()), organization.ID); err != nil {
			return
		}
	}
}

func (s *Service) recordFailure(ctx context.Context, organization Organization, step string, cause error) {
	var terminal terminalError
	attempts := organization.Attempts + 1
	state, code, next := organization.State, "", formatTime(s.config.now().Add(min(time.Duration(1<<min(attempts, 6))*5*time.Second, 5*time.Minute)))
	switch {
	case errors.Is(cause, errCapacity):
		state, code, next = "failed", "capacity", ""
	case errors.As(cause, &terminal):
		state, code, next = "failed", terminal.code, ""
	case attempts >= s.config.Allocation.RetryLimit:
		state, code, next = "failed", step+"_failed", ""
	}
	s.config.Logger.Warn("organization provisioning step failed", "organization", organization.ID, "step", step, "attempt", attempts)
	if _, err := s.registry.store.db.ExecContext(ctx, "UPDATE organizations SET state = ?, attempts = ?, next_attempt_at = ?, error_code = ?, updated_at = ? WHERE id = ?",
		state, attempts, next, code, formatTime(s.config.now()), organization.ID); err != nil {
		s.config.Logger.Warn("organization provisioning state could not be recorded", "organization", organization.ID)
	}
}

func (s *Service) runStep(ctx context.Context, organization *Organization, step string) error {
	allocation := s.config.Allocation
	switch step {
	case "admission":
		if err := s.admit(ctx, organization.ID); err != nil {
			return err
		}
		organization.State = "allocating"
		return nil
	case "provider_organization":
		if organization.ProviderID != "" {
			return nil
		}
		created, err := s.config.Provider.CreateOrganization(ctx, organization.ID, organization.Name)
		if err != nil {
			return err
		}
		if created.ExternalID != "" && created.ExternalID != organization.ID || !safeID(created.ID) {
			return terminalError{code: "provider_organization_conflict"}
		}
		organization.ProviderID = created.ID
		return nil
	case "owner_membership":
		memberships, err := s.config.Provider.Memberships(ctx, organization.CreatorSubject, organization.ProviderID)
		if err != nil {
			return err
		}
		for _, membership := range memberships {
			if membership.UserID == organization.CreatorSubject && membership.OrganizationID == organization.ProviderID && membership.Status == "active" {
				if membership.Role.Slug != "owner" {
					return terminalError{code: "owner_conflict"}
				}
				return nil
			}
		}
		_, err = s.config.Provider.CreateMembership(ctx, organization.CreatorSubject, organization.ProviderID, "owner")
		return err
	case "tenant_files":
		for _, directory := range []string{allocation.TenantRoot, allocation.SocketRoot, filepath.Join(allocation.TenantRoot, organization.ID)} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				return err
			}
			err := os.Chmod(directory, 0o700) // #nosec G302 -- directories need the owner execute bit and stay private to the service user.
			if err != nil {
				return err
			}
		}
		return nil
	case "tenant_start":
		if err := allocation.Launcher.Start(ctx, s.tenantSpec(*organization)); err != nil {
			return err
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			status, err := s.serviceRequest(ctx, *organization, "/internal/v1/health", map[string]string{}, nil)
			if err == nil && status == http.StatusNoContent {
				return nil
			}
			if time.Now().After(deadline) {
				return errors.Join(errors.New("tenant did not become healthy"), err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	case "owner_bootstrap":
		status, err := s.serviceRequest(ctx, *organization, "/internal/v1/owner/bootstrap", map[string]string{"organization_name": organization.Name}, func(claims *cloudassert.Claims) {
			claims.Subject, claims.Email = organization.CreatorSubject, organization.CreatorEmail
		})
		switch {
		case err != nil:
			return err
		case status == http.StatusConflict || status == http.StatusForbidden:
			return terminalError{code: "owner_bootstrap_conflict"}
		case status != http.StatusNoContent:
			return errors.New("owner bootstrap failed")
		}
		return nil
	case "publish":
		organization.State = "ready"
		return nil
	}
	return errors.New("unknown provisioning step")
}

func (s *Service) admit(ctx context.Context, id string) error {
	allocation := s.config.Allocation
	var holding, allocating int
	err := s.registry.store.db.QueryRowContext(ctx, `SELECT
  (SELECT count(*) FROM organizations WHERE managed = 1 AND id != ? AND (state IN ('allocating','ready','disabled','deleting') OR state = 'failed' AND step NOT IN ('', 'admission'))),
  (SELECT count(*) FROM organizations WHERE managed = 1 AND id != ? AND state = 'allocating')`, id, id).Scan(&holding, &allocating)
	if err != nil {
		return err
	}
	if holding >= allocation.MaxTenants || allocating >= allocation.MaxConcurrent {
		return errCapacity
	}
	if allocation.MinFreeDiskBytes > 0 {
		if err := os.MkdirAll(allocation.TenantRoot, 0o700); err != nil {
			return err
		}
		if free, ok := freeDiskBytes(allocation.TenantRoot); !ok || free < allocation.MinFreeDiskBytes {
			return errCapacity
		}
	}
	if allocation.MinAvailableMemoryBytes > 0 {
		if available, ok := availableMemoryBytes(); !ok || available < allocation.MinAvailableMemoryBytes {
			return errCapacity
		}
	}
	return nil
}

func (s *Service) signupAllowed(email string) bool {
	allocation := s.config.Allocation
	if len(allocation.AllowedEmails) == 0 && len(allocation.AllowedDomains) == 0 {
		return true
	}
	if listed(allocation.AllowedEmails, email) {
		return true
	}
	_, domain, ok := strings.Cut(strings.ToLower(email), "@")
	return ok && listed(allocation.AllowedDomains, domain)
}

func (s *Service) newOrganizationPage(c echo.Context) error {
	if s.config.Allocation == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
	}
	session, err := s.session(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?return=%2Forganizations")
	}
	key, err := cloudassert.NewID()
	if err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Organization creation is temporarily unavailable")
	}
	if served, err := s.clientShell(c); served || err != nil {
		return err
	}
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "create", Title: "Create organization", Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""), CreationKey: key})
}

func (s *Service) createOrganization(c echo.Context) error {
	if s.config.Allocation == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
	}
	wantJSON := wantsJSON(c)
	session, err := s.session(c)
	if err != nil {
		return s.refuse(c, http.StatusUnauthorized, "unauthenticated", "Sign in to create an organization")
	}
	if !s.csrfValid(c, session, "") {
		return s.refuse(c, http.StatusForbidden, "invalid_csrf", "Reload the page and try again")
	}
	if s.staff(session.Email) || session.Identity.SupportActor != "" {
		return s.refuse(c, http.StatusForbidden, "staff_session", "Staff and support sessions cannot create customer organizations")
	}
	name, key := strings.TrimSpace(c.FormValue("name")), c.FormValue("creation_key")
	if name == "" || len(name) > 120 || len(key) < 16 || len(key) > 128 || !safeID(key) {
		return s.refuse(c, http.StatusUnprocessableEntity, "invalid_name", "Enter an organization name of at most 120 characters")
	}
	if !s.signupAllowed(session.Email) {
		return s.refuse(c, http.StatusForbidden, "not_eligible", "Organization creation is limited to invited pilot accounts")
	}
	fingerprint := sha256.Sum256([]byte(name))
	id, err := s.recordIntent(c.Request().Context(), session, key, hex.EncodeToString(fingerprint[:]), name)
	switch {
	case errors.Is(err, errIntentConflict):
		return s.refuse(c, http.StatusConflict, "intent_conflict", "This creation request was already used with a different name")
	case errors.Is(err, errQuota):
		return s.refuse(c, http.StatusTooManyRequests, "quota_reached", "Your account has reached its organization limit. Open your existing organization from the organization list.")
	case err != nil:
		return s.refuse(c, http.StatusServiceUnavailable, "unavailable", "Organization creation is temporarily unavailable")
	}
	s.wakeAllocator()
	if wantJSON {
		return s.next(c, http.StatusCreated, "/organizations/"+id+"/provisioning", map[string]any{"organization": map[string]string{"id": id, "name": name}})
	}
	return c.Redirect(http.StatusSeeOther, "/organizations/"+id+"/provisioning")
}

var (
	errIntentConflict = errors.New("creation intent conflict")
	errQuota          = errors.New("organization quota reached")
)

func (s *Service) recordIntent(ctx context.Context, session accountSession, key, fingerprint, name string) (string, error) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	tx, err := s.registry.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var existing, existingFingerprint string
	err = tx.QueryRowContext(ctx, "SELECT organization_id, fingerprint FROM organization_intents WHERE subject = ? AND idempotency_key = ?", session.Subject, key).Scan(&existing, &existingFingerprint)
	if err == nil {
		if existingFingerprint != fingerprint {
			return "", errIntentConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var owned int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM organizations WHERE creator_subject = ? AND state != 'deleted'", session.Subject).Scan(&owned); err != nil {
		return "", err
	}
	if owned >= s.config.Allocation.MaxPerIdentity {
		return "", errQuota
	}
	random, err := cloudassert.NewID()
	if err != nil {
		return "", err
	}
	id := "org_" + random[:20]
	now := formatTime(s.config.now())
	if _, err := tx.ExecContext(ctx, "INSERT INTO organizations(id,provider_id,name,state,endpoint,generation,managed,creator_subject,creator_email,created_at,updated_at) VALUES (?,'',?,'requested',?,1,1,?,?,?,?)",
		id, name, "unix:"+filepath.Join(s.config.Allocation.SocketRoot, id+".sock"), session.Subject, strings.ToLower(session.Email), now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO organization_intents(subject,idempotency_key,fingerprint,organization_id,created_at) VALUES (?,?,?,?,?)", session.Subject, key, fingerprint, id, now); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (s *Service) creatorOrganization(c echo.Context) (accountSession, Organization, error) {
	session, err := s.session(c)
	if err != nil {
		return accountSession{}, Organization{}, err
	}
	organization, err := s.registry.Organization(c.Request().Context(), c.Param("organization"))
	if err != nil || !organization.Managed || organization.CreatorSubject != session.Subject || organization.State == "deleted" {
		return accountSession{}, Organization{}, ErrOrganizationNotFound
	}
	return session, organization, nil
}

func provisioningRetryable(organization Organization) bool {
	return organization.State == "failed" && (organization.ErrorCode == "capacity" || strings.HasSuffix(organization.ErrorCode, "_failed"))
}

func provisioningStatus(organization Organization) templates.HostedProvisioning {
	status := templates.HostedProvisioning{ID: organization.ID, Name: organization.Name, State: organization.State, Step: organization.Step, CanResume: provisioningRetryable(organization)}
	switch {
	case organization.ErrorCode == "capacity":
		status.Error = "The service is at capacity. Your request is saved; resume it later without creating another organization."
	case organization.State == "failed":
		status.Error = "Setup stopped (" + organization.ErrorCode + "). Your request and any completed steps are kept."
	}
	return status
}

func (s *Service) provisioningPage(c echo.Context) error {
	session, organization, err := s.creatorOrganization(c)
	if err != nil {
		return s.denied(c, http.StatusNotFound, "This organization is unavailable")
	}
	if organization.State == "ready" {
		return c.Redirect(http.StatusSeeOther, s.organizationHome(organization.ID))
	}
	if served, err := s.clientShell(c); served || err != nil {
		return err
	}
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "provisioning", Title: "Setting up " + organization.Name, Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""), Provisioning: provisioningStatus(organization)})
}

func (s *Service) provisioningJSON(c echo.Context) error {
	_, organization, err := s.creatorOrganization(c)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "Resource was not found"})
	}
	status := provisioningStatus(organization)
	result := map[string]any{"id": status.ID, "name": status.Name, "state": status.State, "step": status.Step, "error": status.Error, "can_resume": status.CanResume}
	if organization.State == "ready" {
		result["next"] = s.organizationHome(organization.ID)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) resumeProvisioning(c echo.Context) error {
	session, organization, err := s.creatorOrganization(c)
	if err != nil {
		return s.refuse(c, http.StatusNotFound, "not_found", "This organization is unavailable")
	}
	if !s.csrfValid(c, session, "") {
		return s.refuse(c, http.StatusForbidden, "invalid_csrf", "Reload the page and try again")
	}
	if !provisioningRetryable(organization) {
		return s.next(c, http.StatusOK, "/organizations/"+organization.ID+"/provisioning", nil)
	}
	state := "allocating"
	if organization.Step == "" {
		state = "requested"
	}
	if _, err := s.registry.store.db.ExecContext(c.Request().Context(), "UPDATE organizations SET state = ?, attempts = 0, next_attempt_at = '', error_code = '', updated_at = ? WHERE id = ? AND state = 'failed'", state, formatTime(s.config.now()), organization.ID); err != nil {
		return s.refuse(c, http.StatusServiceUnavailable, "unavailable", "Setup could not be resumed")
	}
	s.wakeAllocator()
	return s.next(c, http.StatusOK, "/organizations/"+organization.ID+"/provisioning", nil)
}

func (s *Service) pendingOrganizations(ctx context.Context, subject string) ([]templates.HostedOrganizationChoice, error) {
	rows, err := s.registry.store.db.QueryContext(ctx, organizationSelect+" WHERE managed = 1 AND creator_subject = ? AND state IN ('requested','allocating','failed') ORDER BY created_at", subject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []templates.HostedOrganizationChoice
	for rows.Next() {
		organization, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, templates.HostedOrganizationChoice{ID: organization.ID, Name: organization.Name, Status: organization.State})
	}
	return result, rows.Err()
}

func (s *Service) deleteOrganizationPage(c echo.Context) error {
	session, organization, err := s.ownerOrganization(c)
	if err != nil {
		return s.denied(c, http.StatusForbidden, "Only a current owner can delete this organization")
	}
	return s.render(c, http.StatusOK, templates.HostedPageData{Mode: "delete", Title: "Delete " + organization.Name, Email: session.Email, CSRF: cloudassert.CSRFToken(session.CSRFSecret, organization.ID),
		OrganizationID: organization.ID, OrganizationName: organization.Name})
}

func (s *Service) ownerOrganization(c echo.Context) (accountSession, Organization, error) {
	ctx := c.Request().Context()
	session, err := s.session(c)
	if err != nil {
		return accountSession{}, Organization{}, err
	}
	organization, err := s.readyOrganization(ctx, c.Param("organization"))
	if err != nil || !organization.Managed {
		return accountSession{}, Organization{}, ErrOrganizationNotFound
	}
	authorized, err := s.auth.authorization(ctx, session, organization.ID)
	if err != nil || authorized.Support {
		return accountSession{}, Organization{}, errNoSession
	}
	current, err := s.config.Provider.CurrentSession(ctx, authorized.Identity)
	if err != nil || current.Subject != session.Subject || current.OrganizationID != organization.ProviderID {
		return accountSession{}, Organization{}, errNoSession
	}
	memberships, err := s.config.Provider.Memberships(ctx, session.Subject, organization.ProviderID)
	if err != nil {
		return accountSession{}, Organization{}, err
	}
	for _, membership := range memberships {
		if membership.UserID == session.Subject && membership.Status == "active" && membership.Role.Slug == "owner" {
			return session, organization, nil
		}
	}
	return accountSession{}, Organization{}, errNoSession
}

func (s *Service) deleteOrganization(c echo.Context) error {
	ctx := c.Request().Context()
	session, organization, err := s.ownerOrganization(c)
	if err != nil {
		return s.denied(c, http.StatusForbidden, "Only a current owner can delete this organization")
	}
	if !s.csrfValid(c, session, organization.ID) || c.FormValue("confirm") != organization.Name {
		return s.denied(c, http.StatusUnprocessableEntity, "Type the organization name exactly to confirm deletion")
	}
	if _, err := s.registry.store.db.ExecContext(ctx, "UPDATE organizations SET state = 'deleting', updated_at = ? WHERE id = ? AND state = 'ready'", formatTime(s.config.now()), organization.ID); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Deletion could not start")
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := s.finishDeletion(detached, organization.ID); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "Deletion is pending; it resumes automatically")
	}
	if err := s.auth.audit(detached, session.Subject, organization.ID, "organization_deleted"); err != nil {
		s.config.Logger.Warn("organization deletion audit failed", "organization", organization.ID)
	}
	return c.Redirect(http.StatusSeeOther, "/organizations")
}

func (s *Service) finishDeletion(ctx context.Context, id string) error {
	revoked, err := s.auth.revokeOrganization(ctx, id)
	if err != nil {
		return err
	}
	s.revokeAtTenants(ctx, revoked)
	if err := s.config.Allocation.Launcher.Stop(id); err != nil {
		return err
	}
	_, err = s.registry.store.db.ExecContext(ctx, "UPDATE organizations SET state = 'deleted', updated_at = ? WHERE id = ? AND state = 'deleting'", formatTime(s.config.now()), id)
	return err
}

func (s *Service) deletingOrganizations(ctx context.Context) ([]string, error) {
	rows, err := s.registry.store.db.QueryContext(ctx, "SELECT id FROM organizations WHERE managed = 1 AND state = 'deleting'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) resumeDeletions(ctx context.Context) {
	ids, err := s.deletingOrganizations(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		if err := s.finishDeletion(ctx, id); err != nil {
			s.config.Logger.Warn("organization deletion is still pending", "organization", id)
		}
	}
}

func (a *authStore) revokeOrganization(ctx context.Context, organization string) ([]authorization, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	items, err := activeAuthorizations(ctx, tx, "SELECT binding,organization_id,identity_json,expires_at,support,effective_email FROM authorizations WHERE organization_id = ? AND revoked_at IS NULL", organization)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE authorizations SET revoked_at = ? WHERE organization_id = ? AND revoked_at IS NULL", formatTime(a.now()), organization); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
