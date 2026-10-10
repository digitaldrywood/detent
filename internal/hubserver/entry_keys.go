package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) entryKeyCredential(ctx context.Context, claims cloudassert.Claims) (apiCredential, int, error) {
	if !s.hostedShared() || claims.Kind != cloudassert.KindKey || claims.Key == nil || !claims.Key.Valid() || !claims.ExpiresAt.After(s.config.now()) || claims.Audience != s.config.Hosted.OrganizationID {
		return apiCredential{}, http.StatusUnauthorized, auth.ErrHostedIdentity
	}
	provider, err := s.hostedProviderOrganization(ctx)
	if err != nil || provider != claims.ProviderOrganization {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	identity := &auth.HostedIdentity{Subject: claims.Subject, OrganizationID: provider, CreatedAt: claims.IssuedAt, ExpiresAt: claims.ExpiresAt, SessionID: claims.Key.ID}
	credential := apiCredential{EntryKey: claims.Key, Hosted: identity, HostedRole: claims.Role, HostedKeyScope: claims.Key.Permission, HostedProjectAccess: hostedProjectsAll, ID: claims.Key.ID, Hash: apikey.HashToken(claims.Key.ID), NativeOnly: true, Scope: apiScopeOperator}
	if claims.Key.Permission == apikey.ScopeAdmin {
		credential.Scope = apiScopeAdmin
	}
	if claims.Key.Kind == "service" {
		token, err := s.config.generateToken()
		if err != nil {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		credential.ID = "service_" + claims.Key.ID
		_, err = s.database.db.ExecContext(ctx, "INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES(?,?,?,'','operator',1,?,?) ON CONFLICT(id) DO NOTHING", credential.ID, "Organization service principal", apikey.HashToken(token), formatHubTime(s.config.now()), formatHubTime(s.config.now()))
		if err != nil {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		credential.HostedPrincipal = credential.ID
		return credential, http.StatusOK, nil
	}
	var local string
	err = s.database.auth().QueryRowContext(ctx, `SELECT m.membership_id,m.principal_id,m.role FROM hosted_members m JOIN api_tokens t ON t.id=m.principal_id WHERE m.user_id=? AND m.active=1 AND t.revoked_at IS NULL`, claims.Subject).Scan(&credential.HostedMembership, &credential.HostedPrincipal, &local)
	if err != nil || !auth.ValidOrganizationRole(local) {
		return apiCredential{}, http.StatusForbidden, &apikey.Refusal{Code: "membership_denied", Message: "The key owner is not an active organization member"}
	}
	credential.HostedRole = lesserHostedRole(local, claims.Role)
	credential.ID = credential.HostedPrincipal
	return credential, http.StatusOK, nil
}

func (s *Service) auditEntryKey(c echo.Context, claims cloudassert.Claims) error {
	metadata, err := json.Marshal(struct {
		KeyID string `json:"key_id"`
		Owner string `json:"owner"`
		Kind  string `json:"kind"`
	}{claims.Key.ID, claims.Subject, claims.Key.Kind})
	if err != nil {
		return err
	}
	_, err = s.database.db.ExecContext(c.Request().Context(), `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,mutation_json) VALUES(?,?,?,?,'','key_call',?,'',0,?,?,?,?)`, claims.Audience, claims.Key.ID, claims.Subject, claims.Subject, claims.Method+" "+claims.Path, formatHubTime(claims.IssuedAt), formatHubTime(claims.ExpiresAt), formatHubTime(s.config.now()), string(metadata))
	return err
}

func (s *Service) entryKeyCatalog(c echo.Context) error {
	definitions, err := (hostedOperatorExecutor{s}).ListTools(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, definitions)
}

func (s *Service) entryKeyAdmin(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" || claims.Role != "owner" && claims.Role != "admin" {
		return c.NoContent(http.StatusForbidden)
	}
	var role string
	err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT m.role FROM hosted_members m JOIN api_tokens t ON t.id=m.principal_id WHERE m.user_id=? AND m.active=1 AND t.revoked_at IS NULL", claims.Subject).Scan(&role)
	if err != nil || role != "owner" && role != "admin" {
		return c.NoContent(http.StatusForbidden)
	}
	return c.NoContent(http.StatusOK)
}

func (s *Service) entryKeyCall(c echo.Context) error {
	var call struct {
		Name         string          `json:"name"`
		Arguments    json.RawMessage `json:"arguments"`
		ConnectionID string          `json:"connection_id"`
	}
	if err := decodeAPIJSON(c, &call); err != nil {
		return invalidAPIRequest(c, err)
	}
	if call.ConnectionID == "" || len(call.ConnectionID) > 256 {
		return invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	ctx := operatortool.BindConnection(c.Request().Context(), call.ConnectionID, "shared-entry")
	if err := (hostedOperatorExecutor{s}).OpenConnection(ctx); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, _, err := s.authenticateAPIRequest(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	definition, found := operatortool.Lookup(call.Name)
	if !found {
		return c.JSON(http.StatusForbidden, apikey.Refusal{Code: "tool_unavailable", Message: "This tool is unavailable in the target organization"})
	}
	permission := projectToolScope(call.Name, definition.Annotations.ReadOnly)
	if operatortool.IsAdministration(call.Name) {
		permission = operatortool.AdministrationScope(call.Name)
	}
	if !hostedKeyAllows(credential.HostedKeyScope, permission) {
		return c.JSON(http.StatusForbidden, apikey.Refusal{Code: "key_permission_denied", Message: "The key permission does not allow this call"})
	}
	if !hostedRoleAllows(credential.HostedRole, permission) {
		return c.JSON(http.StatusForbidden, apikey.Refusal{Code: "role_denied", Message: "The current organization role does not allow this call"})
	}
	var selector struct {
		ProjectID string `json:"project_id"`
	}
	if json.Unmarshal(call.Arguments, &selector) != nil {
		return invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	if selector.ProjectID != "" && !credential.EntryKey.Allows(selector.ProjectID) {
		return c.JSON(http.StatusForbidden, apikey.Refusal{Code: "key_project_denied", Message: "The project is outside this key's access context"})
	}
	definitions, err := (hostedOperatorExecutor{s}).ListTools(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	available := false
	for _, d := range definitions {
		available = available || d.Name == call.Name
	}
	if !available {
		return c.JSON(http.StatusForbidden, apikey.Refusal{Code: "tool_unavailable", Message: "The target organization's role or plan does not provide this tool"})
	}
	result, err := (hostedOperatorExecutor{s}).Execute(ctx, operatortool.Call{Name: call.Name, Arguments: call.Arguments})
	if err != nil {
		var refusal *apikey.Refusal
		if errors.As(err, &refusal) {
			return c.JSON(http.StatusForbidden, refusal)
		}
		return s.nativeAPIError(c, err)
	}
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, result.Content)
}

func (credential apiCredential) entryKeyGrantSQL(organization, project string) (string, []any) {
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

func (s *Service) entryKeyProjects(c echo.Context) error {
	credential, _, err := s.authenticateAPIRequest(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	projects, err := s.hostedReadableProjects(c.Request().Context(), credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, projects)
}

func (s *Service) entryKeyAudit(c echo.Context) error {
	var event struct {
		KeyID string `json:"key_id"`
		Owner string `json:"owner"`
		Event string `json:"event"`
	}
	if err := decodeAPIJSON(c, &event); err != nil {
		return invalidAPIRequest(c, err)
	}
	if len(event.KeyID) > 256 || event.Owner == "" || len(event.Owner) > 256 || event.Event == "" || len(event.Event) > 128 {
		return invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return c.NoContent(http.StatusForbidden)
	}
	metadata, err := json.Marshal(event)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	_, err = s.database.db.ExecContext(c.Request().Context(), `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,mutation_json) VALUES(?,?,?,?,'',?,?,'',0,?,?,?,?)`, claims.Audience, event.KeyID, event.Owner, event.Owner, event.Event, claims.Path, formatHubTime(claims.IssuedAt), formatHubTime(claims.ExpiresAt), formatHubTime(s.config.now()), string(metadata))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
