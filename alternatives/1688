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
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) entryKeyCredential(ctx context.Context, claims cloudassert.Claims) (apiCredential, int, error) {
	return s.entryAccessKeyCredential(ctx, claims)
}

type entryKeyAuditContext struct{}

type entryKeyAudit struct {
	claims                       cloudassert.Claims
	operation, project, resource string
	rowID                        int64
}

func entryKeyAuditFrom(ctx context.Context) *entryKeyAudit {
	audit, ok := ctx.Value(entryKeyAuditContext{}).(*entryKeyAudit)
	if !ok {
		return nil
	}
	return audit
}

func (a *entryKeyAudit) readOnly() bool {
	if a.operation == http.MethodPost+" /internal/v1/keys/catalog" {
		return true
	}
	if definition, found := operatortool.Lookup(a.operation); found {
		return definition.Annotations.ReadOnly
	}
	method, _, _ := strings.Cut(a.operation, " ")
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func (s *Service) auditEntryKey(ctx context.Context, _ hostedExecer, audit *entryKeyAudit, status int) error {
	return s.recordEntryAccessKeyAudit(ctx, audit, status)
}

func (s *Service) entryKeyTools(ctx context.Context) ([]operatortool.Definition, error) {
	if s.config.TenantRepository == nil {
		return (hostedOperatorExecutor{s}).ListTools(ctx)
	}
	definitions, err := nativeOperatorExecutor{service: s}.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := (hubProjectExecutor{service: s}).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	fleet, err := (hubFleetExecutor{service: s}).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	definitions = append(definitions, projects...)
	definitions = append(definitions, fleet...)
	workspace, err := (workspaceOperatorExecutor{server: s}).ListTools(ctx)
	if err != nil {
		return nil, err
	}
	for _, definition := range workspace {
		if entryKeyWorkspaceTool(definition.Name) {
			definitions = append(definitions, definition)
		}
	}

	definitions = append(definitions, (hostedContextExecutor{service: s}).listTools(ctx)...)
	if catalog, err := operatorCatalog(ctx); err == nil && catalog.credential.HostedRole != "account" {
		for _, definition := range operatortool.ThreadCatalog() {
			required := apikey.ScopeRead
			if !definition.Annotations.ReadOnly {
				required = apikey.ScopeWrite
			}
			if _, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: required, Action: definition.Name}); err == nil {
				definitions = append(definitions, definition)
			}
		}
	}
	if _, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, Action: operatortool.ListChanges}); err == nil {
		definition, _ := operatortool.Lookup(operatortool.ListChanges)
		definitions = append(definitions, definition)
	}
	for _, name := range []string{operatortool.CreateChange, operatortool.PublishChangeVersion, operatortool.ReviewChange} {
		required := apikey.ScopeWrite
		if name == operatortool.ReviewChange && s.config.Hosted != nil {
			required = apikey.ScopeAdmin
		}
		if _, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: required, ResourceKind: "work_item", Action: name}); err == nil {
			definition, _ := operatortool.ChangeDefinition(name)
			definitions = append(definitions, definition)
		}
	}
	supported := make([]operatortool.Definition, 0)
	seen := make(map[string]bool)
	for _, definition := range definitions {
		if entryKeyToolSupported(definition.Name) && !seen[definition.Name] {
			seen[definition.Name] = true
			supported = append(supported, definition)
		}
	}
	return supported, nil
}

func (s *Service) entryKeyToolAvailable(ctx context.Context, name string) (bool, error) {
	if s.config.TenantRepository == nil {
		definitions, err := s.entryKeyTools(ctx)
		if err != nil {
			return false, err
		}
		return entryKeyHasDefinition(definitions, name), nil
	}
	if entryKeyWorkspaceTool(name) {
		definitions, err := (workspaceOperatorExecutor{server: s}).ListTools(ctx)
		return entryKeyHasDefinition(definitions, name), err
	}
	if operatortool.IsAttachmentTool(name) {
		required := apikey.ScopeWrite
		if name == operatortool.ReadAttachment || name == operatortool.ReadAttachmentMetadata {
			required = apikey.ScopeRead
		}
		_, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: required, ResourceKind: "work_item", Action: name})
		return err == nil, nil
	}
	if !entryKeyToolSupported(name) {
		return false, nil
	}
	if _, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return false, err
	}
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		return false, err
	}
	switch name {
	case operatortool.WorkList, operatortool.WorkItem:
		_, found := entryKeyDefinition(operatortool.WorkReadCatalog(), name)
		return found && catalog.credential.HostedRole != "account", nil
	case operatortool.MoveItem, operatortool.FileIssue, operatortool.EditItem, operatortool.AddComment, operatortool.SetDependency:
		definition, found := entryKeyDefinition(operatortool.CommandCatalog(), name)
		if !found {
			return false, nil
		}
		_, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: projectToolScope(name, definition.Annotations.ReadOnly), ResourceKind: "work_item", Action: name})
		return err == nil, nil
	case "list_projects", "get_native_project", "get_project_policy":
		definitions := append(operatortool.ProjectCatalog(), operatortool.LocalProjectCatalog()...)
		definition, found := entryKeyDefinition(definitions, name)
		if !found {
			return false, nil
		}
		requirement := operatortool.Requirement{Scope: projectToolScope(name, definition.Annotations.ReadOnly)}
		if requirement.Scope == apikey.ScopeWrite {
			requirement.ResourceKind = "project"
		}
		_, err := s.authorizeCatalog(ctx, requirement)
		return err == nil, nil
	case operatortool.AppUpdates, operatortool.AppBootstrapPayload:
		if s.config.Hosted == nil || s.database == nil || catalog.credential.Hosted == nil || catalog.credential.HostedRole == "account" || operatorIdentity(catalog.credential, s.config.Hosted.OrganizationID) != operatortool.ConnectionIdentity(ctx) {
			return false, nil
		}
		_, found := entryKeyDefinition(operatortool.HostedContextCatalog(), name)
		if !found {
			return false, nil
		}
		requirement := operatortool.Requirement{Scope: apikey.ScopeRead}
		if name == operatortool.AppUpdates {
			requirement.ResourceKind = "runners"
			if !s.appAllRunnerGrants(ctx, catalog.credential) {
				return false, nil
			}
		}
		_, err := s.authorizeCatalog(ctx, requirement)
		return err == nil, nil
	case "list_threads", "get_thread", "create_thread", "post_to_thread", "resolve_thread":
		if catalog.credential.HostedRole == "account" {
			return false, nil
		}
		if _, found := entryKeyDefinition(operatortool.ThreadCatalog(), name); !found {
			return false, nil
		}
		required := apikey.ScopeRead
		if name != "list_threads" && name != "get_thread" {
			required = apikey.ScopeWrite
		}
		_, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: required, Action: name})
		return err == nil, nil
	case operatortool.ListComments:
		if _, found := operatortool.Lookup(name); !found {
			return false, nil
		}
		_, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ResourceKind: "comment", Action: name})
		return err == nil, nil
	case operatortool.CreateChange, operatortool.PublishChangeVersion, operatortool.ReviewChange:
		required := apikey.ScopeWrite
		if name == operatortool.ReviewChange && s.config.Hosted != nil {
			required = apikey.ScopeAdmin
		}
		_, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: required, ResourceKind: "work_item", Action: name})
		return err == nil, nil
	case operatortool.ListChanges:
		if _, found := operatortool.Lookup(name); !found {
			return false, nil
		}
		_, err := s.authorizeCatalog(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, Action: name})
		return err == nil, nil
	default:
		return false, nil
	}
}

func entryKeyToolSupported(name string) bool {
	if entryKeyWorkspaceTool(name) || operatortool.IsAttachmentTool(name) {
		return true
	}
	switch name {
	case "list_projects", "get_native_project", "get_project_policy", operatortool.WorkList, operatortool.WorkItem, operatortool.AppUpdates, operatortool.AppBootstrapPayload, operatortool.MoveItem, operatortool.FileIssue, operatortool.EditItem, operatortool.AddComment, operatortool.SetDependency, operatortool.ListComments, operatortool.ListChanges, operatortool.CreateChange, operatortool.PublishChangeVersion, operatortool.ReviewChange, "list_threads", "get_thread", "create_thread", "post_to_thread", "resolve_thread":
		return true
	default:
		return false
	}
}

func entryKeyDefinition(definitions []operatortool.Definition, name string) (operatortool.Definition, bool) {
	for _, definition := range definitions {
		if definition.Name == name {
			return definition, true
		}
	}
	return operatortool.Definition{}, false
}

func entryKeyHasDefinition(definitions []operatortool.Definition, name string) bool {
	_, found := entryKeyDefinition(definitions, name)
	return found
}

func (s *Service) entryKeyCatalog(c echo.Context) error {
	definitions, err := s.entryKeyTools(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, definitions)
}

func (s *Service) entryKeyAdmin(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" || claims.Role != "owner" && claims.Role != "admin" {
		return s.hostedJSONError(c, http.StatusForbidden, "Organization administrator service access is required")
	}
	member, err := s.activeAccessMember(ctx, claims.Subject)
	role := member.Role
	if err != nil || role != "owner" && role != "admin" {
		return s.hostedJSONError(c, http.StatusForbidden, "Active organization administrator membership is required")
	}
	return c.NoContent(http.StatusOK)
}

func (s *Service) entryKeyCall(c echo.Context) error {
	var call struct {
		Name         string          `json:"name"`
		Arguments    json.RawMessage `json:"arguments"`
		ConnectionID string          `json:"connection_id"`
		ClientName   string          `json:"client_name,omitempty"`
	}
	if err := decodeAPIJSON(c, &call); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if audit := entryKeyAuditFrom(c.Request().Context()); audit != nil {
		audit.operation = call.Name
	}
	if call.ConnectionID == "" || len(call.ConnectionID) > 256 || len(call.ClientName) > 256 {
		return s.invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	if call.ClientName == "" {
		call.ClientName = "shared-entry"
	}
	ctx := operatortool.BindConnection(c.Request().Context(), call.ConnectionID, call.ClientName)
	if call.Name != operatortool.WorkList {
		if err := (hostedOperatorExecutor{s}).OpenConnection(ctx); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	credential, _, err := s.authenticateAPIRequest(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	definition, found := operatortool.Lookup(call.Name)
	if !found {
		return s.refusedAPIError(c, http.StatusForbidden, "tool_unavailable", "This tool is unavailable in the target organization", apikey.Refusal{Code: "tool_unavailable", Message: "This tool is unavailable in the target organization"})
	}
	permission := projectToolScope(call.Name, definition.Annotations.ReadOnly)
	if operatortool.IsAdministration(call.Name) {
		permission = operatortool.AdministrationScope(call.Name)
	}
	if !hostedKeyAllows(credential.HostedKeyScope, permission) {
		return s.refusedAPIError(c, http.StatusForbidden, "key_permission_denied", "The key permission does not allow this call", apikey.Refusal{Code: "key_permission_denied", Message: "The key permission does not allow this call"})
	}
	if !hostedRoleAllows(credential.HostedRole, permission, call.Name) {
		return s.refusedAPIError(c, http.StatusForbidden, "role_denied", "The current organization role does not allow this call", apikey.Refusal{Code: "role_denied", Message: "The current organization role does not allow this call"})
	}
	var selector struct {
		ProjectID  string `json:"project_id"`
		Identifier string `json:"identifier"`
		Reference  string `json:"reference"`
	}
	if json.Unmarshal(call.Arguments, &selector) != nil {
		return s.invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	resource := selector.Identifier
	if resource == "" {
		resource = selector.Reference
	}
	if audit := entryKeyAuditFrom(ctx); audit != nil {
		audit.project, audit.resource = selector.ProjectID, resource
	}
	if selector.ProjectID != "" && !credential.EntryKey.Allows(selector.ProjectID) {
		return s.refusedAPIError(c, http.StatusForbidden, "key_project_denied", "The project is outside this key's access context", apikey.Refusal{Code: "key_project_denied", Message: "The project is outside this key's access context"})
	}
	available, err := s.entryKeyToolAvailable(ctx, call.Name)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if !available {
		return s.refusedAPIError(c, http.StatusForbidden, "tool_unavailable", "The target organization's role or plan does not provide this tool", apikey.Refusal{Code: "tool_unavailable", Message: "The target organization's role or plan does not provide this tool"})
	}
	if audit := entryKeyAuditFrom(ctx); audit != nil {
		if err := s.auditEntryKey(ctx, nil, audit, 0); err != nil {
			return s.internalAPIError(c, "audit_unavailable", "Key audit is unavailable", err)
		}
	}
	result, err := (hostedOperatorExecutor{s}).Execute(ctx, operatortool.Call{Name: call.Name, Arguments: call.Arguments})
	if err != nil {
		if errors.Is(err, mutation.ErrConflict) {
			return s.nativeAPIError(c, nativeConflict(0))
		}
		var request *operatortool.RequestError
		if errors.As(err, &request) {
			return s.refusedAPIError(c, http.StatusUnprocessableEntity, request.Code, request.Message, apiErrorResponse{Code: request.Code, Message: request.Message})
		}
		if errors.Is(err, operatortool.ErrInvalidArguments) {
			return s.invalidAPIRequest(c, err)
		}
		if errors.Is(err, chat.ErrActionNotFound) {
			return s.nativeAPIError(c, nativeNotFound())
		}
		var refusal *apikey.Refusal
		if errors.As(err, &refusal) {
			return s.nativeAPIError(c, refusal)
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
		return s.invalidAPIRequest(c, err)
	}
	if len(event.KeyID) > 256 || event.Owner == "" || len(event.Owner) > 256 || event.Event == "" || len(event.Event) > 128 {
		return s.invalidAPIRequest(c, operatortool.ErrInvalidArguments)
	}
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService {
		return s.hostedJSONError(c, http.StatusForbidden, "Shared service access is required to record key audit events")
	}
	now := s.config.now()
	ctx := c.Request().Context()
	err := s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		_, err := w.RecordAccessAudit(ctx, accessstore.Audit{OrganizationID: s.accessOrganization(), SessionID: event.KeyID, ActualActor: event.Owner, EffectiveUser: event.Owner, Event: event.Event, Route: claims.Path, StartedAt: claims.IssuedAt, ExpiresAt: claims.ExpiresAt, RecordedAt: now, RecordedAtNS: now.UnixNano(), KeyID: event.KeyID, KeyOwner: event.Owner})
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) entryKeyContext(c echo.Context) error {
	claims, ok := hostedSharedClaims(c)
	if !ok || claims.Kind != cloudassert.KindService || claims.Subject == "" {
		return s.hostedJSONError(c, http.StatusForbidden, "Shared service identity is required to read key context")
	}
	member, err := s.activeAccessMember(c.Request().Context(), claims.Subject)
	local := member.Role
	if err != nil || !auth.ValidOrganizationRole(local) || !auth.ValidOrganizationRole(claims.Role) {
		return s.hostedJSONError(c, http.StatusForbidden, "Active organization membership and a valid role are required")
	}
	credential := apiCredential{Hosted: &auth.HostedIdentity{Subject: claims.Subject}, HostedRole: lesserHostedRole(local, claims.Role)}
	projects, err := s.hostedReadableProjects(c.Request().Context(), credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, struct {
		Role     string                `json:"role"`
		Projects []appBootstrapProject `json:"projects"`
	}{credential.HostedRole, projects})
}

func entryKeyWorkspaceTool(name string) bool {
	if name == operatortool.ActionResult || name == operatortool.ConnectionInfo {
		return true
	}
	if _, err := operatortool.WorkspaceDefinition(name); err != nil {
		return false
	}
	return strings.Contains(name, "conversation") || name == "list_work_item_references"
}
