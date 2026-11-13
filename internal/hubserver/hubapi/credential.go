package hubapi

import (
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type APIScope string

const APIScopeWorker APIScope = "worker"

const APIScopeOperator APIScope = "operator"

const APIScopeAdmin APIScope = "admin"

type APICredential struct {
	EntryKey   *apikey.KeyAuthority
	Hosted     *auth.HostedIdentity
	HostedRole string
	// HostedMembership is the provider membership the credential was
	// resolved from; mutation rechecks compare it against the member row.
	HostedMembership    string
	HostedPrincipal     string
	HostedKeyScope      apikey.Scope
	HostedKeyKind       string
	HostedProjectAccess HostedProjectAccess
	ManageRunners       bool
	SessionHash         string
	Hash                string
	ID                  string
	Name                string
	Scope               APIScope
	NativeOnly          bool
	Runner              runnerauth.Identity
	// Agent is the assistant acting for this credential's principal.
	Agent string
	// RunnerRenewal is set only for POST renewal of this bound identity.
	// Expiry limits ordinary API use, but must not strand a stopped host.
	RunnerRenewal   bool `json:"-"`
	RequestCapacity *int `json:"-"`
}

type APIErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TokenRequest struct {
	Kind          string              `json:"-"`
	Name          string              `json:"name"`
	Scope         APIScope            `json:"scope"`
	Issuer        *APICredential      `json:"-"`
	KeyScope      apikey.Scope        `json:"-"`
	ExpiresAt     *time.Time          `json:"-"`
	ProjectIDs    []string            `json:"-"`
	ProjectAccess HostedProjectAccess `json:"-"`
}

type TokenGrantResponse struct {
	OrganizationID string `json:"organization_id"`
	ProjectID      string `json:"project_id"`
}

type TokenResponse struct {
	Kind          string               `json:"kind,omitempty"`
	ProjectAccess HostedProjectAccess  `json:"project_access,omitempty"`
	Projects      []string             `json:"project_ids"`
	ExpiresAt     *time.Time           `json:"expires_at,omitempty"`
	KeyScope      apikey.Scope         `json:"key_scope,omitempty"`
	NativeOnly    bool                 `json:"native_only"`
	RevokedAt     *time.Time           `json:"revoked_at,omitempty"`
	Grants        []TokenGrantResponse `json:"grants"`
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Scope         APIScope             `json:"scope"`
	Token         string               `json:"token,omitempty"`
	Fingerprint   string               `json:"fingerprint"`
	CreatedAt     time.Time            `json:"created_at"`
	RotatedAt     time.Time            `json:"rotated_at,omitempty"`
}

func TokenFingerprint(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func (credential APICredential) entryKeyGrantSQL(organization, project string) (string, []any) {
	condition, args := "1=1", []any{}
	if credential.EntryKey.Kind == "personal" {
		condition = "EXISTS (SELECT 1 FROM hosted_project_grants h JOIN hosted_members m ON m.user_id=h.user_id JOIN api_tokens t ON t.id=m.principal_id WHERE m.active=1 AND t.revoked_at IS NULL AND h.user_id=? AND h.organization_id=" + organization + " AND h.project_id=" + project + ")"
		args = append(args, credential.Hosted.Subject)
	}
	var query strings.Builder
	query.WriteString(condition)
	for _, selection := range []struct {
		ids      []string
		excluded bool
	}{{credential.EntryKey.Projects, false}, {credential.EntryKey.BlockedProjects, true}} {
		if len(selection.ids) == 0 {
			continue
		}
		operator := " IN "
		if selection.excluded {
			operator = " NOT IN "
		}
		query.WriteString(" AND " + project + operator + "(" + strings.TrimSuffix(strings.Repeat("?,", len(selection.ids)), ",") + ")")
		for _, id := range selection.ids {
			args = append(args, id)
		}
	}
	return query.String(), args
}

type HostedProjectAccess string

const (
	HostedProjectsAll      HostedProjectAccess = "all"
	HostedProjectsSelected HostedProjectAccess = "selected"
)

func (credential APICredential) ProjectGrantSQL(organization, project string) (string, []any) {
	live := "EXISTS (SELECT 1 FROM projects live_project WHERE live_project.organization_id=" + organization + " AND live_project.id=" + project + " AND live_project.deleted_at IS NULL)"
	return credential.ProjectAccessSQL(organization, project, live)
}

func (credential APICredential) ProjectAccessSQL(organization, project, live string) (string, []any) {
	if credential.EntryKey != nil {
		condition, args := credential.entryKeyGrantSQL(organization, project)
		return live + " AND (" + condition + ")", args
	}
	if credential.Scope == APIScopeAdmin && !credential.NativeOnly {
		return live, nil
	}
	if credential.Runner.RunnerID != "" {
		return "EXISTS (SELECT 1 FROM runner_identities r WHERE r.token_id=? AND " + RunnerProjectAccessSQL(organization, project) + ")", []any{credential.ID}
	}
	principal := credential.ID
	if credential.Hosted != nil && credential.HostedKeyScope != "" && credential.HostedProjectAccess == HostedProjectsAll {
		principal = credential.HostedPrincipal
	}
	condition := live + " AND EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=? AND g.organization_id=" + organization + " AND g.project_id=" + project + ")"
	args := []any{principal}
	if credential.Hosted != nil && credential.HostedKeyScope != "" && credential.HostedProjectAccess == HostedProjectsSelected {
		condition += " AND EXISTS (SELECT 1 FROM token_grants u WHERE u.token_id=? AND u.organization_id=" + organization + " AND u.project_id=" + project + ")"
		args = append(args, credential.HostedPrincipal)
	}
	if credential.Hosted != nil {
		condition += " AND EXISTS (SELECT 1 FROM hosted_project_grants h JOIN hosted_members m ON m.user_id=h.user_id WHERE m.active=1 AND h.user_id=? AND h.organization_id=" + organization + " AND h.project_id=" + project + ")"
		args = append(args, credential.Hosted.Subject)
	}
	return condition, args
}

func RunnerTimeValid(now time.Time, created, expiry string) bool {
	start, err := ParseTimeValue(created)
	if err != nil {
		return false
	}
	end, err := ParseTimeValue(expiry)
	return err == nil && !now.Before(start) && now.Before(end)
}

func RunnerProjectAccessSQL(organization, project string) string {
	return "r.organization_id=" + organization + " AND r.removed_at IS NULL AND (r.scope='organization' OR EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.organization_id=" + organization + " AND g.project_id=" + project + "))"
}
