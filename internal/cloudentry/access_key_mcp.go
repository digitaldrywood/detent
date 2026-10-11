package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type accessKeyConnection struct{ hash string }
type accessKeyContext struct{}
type accessKeyExecutor struct{ service *Service }

func (s *Service) registerKeyMCP() {
	s.keyMCP = mcp.NewHTTPHandler(accessKeyExecutor{s}, s.config.Build.Version, mcp.HTTPConfig{Logger: s.config.Logger, Principal: func(request *http.Request) operatortool.Identity {
		return operatortool.ConnectionIdentity(request.Context())
	}})
	s.echo.Any("/mcp", echo.WrapHandler(s.keyMCP), s.accessKeyAuthority)
	s.echo.GET("/api/v2/organizations", s.keyOrganizationsJSON, s.accessKeyAuthority)
	s.echo.Any("/api/v2/*", s.keyAPI)
}

func (s *Service) accessKeyAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		hash, err := keyBearer(c)
		if err != nil {
			return keyError(c, err)
		}
		key, err := s.activeAccessKey(c.Request().Context(), hash)
		if err != nil {
			return keyError(c, err)
		}
		identity := operatortool.Identity{PrincipalID: key.Owner, OrganizationID: "account:" + key.Owner, CredentialID: key.ID}
		if key.Kind == "service" {
			identity.PrincipalID, identity.OrganizationID = "service:"+key.ID, key.ServiceOrganization
		}
		ctx := context.WithValue(c.Request().Context(), accessKeyContext{}, accessKeyConnection{hash: hash})
		ctx = operatortool.WithConnection(ctx, operatortool.Connection{Identity: identity, DashboardURL: s.config.PublicURL})
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}

func (e accessKeyExecutor) current(ctx context.Context) (accessKey, error) {
	connection, ok := ctx.Value(accessKeyContext{}).(accessKeyConnection)
	if !ok {
		return accessKey{}, operatortool.ErrAccessDenied
	}
	return e.service.activeAccessKey(ctx, connection.hash)
}

func (s *Service) keyOrganizations(ctx context.Context, key accessKey) ([]Organization, error) {
	organizations := []Organization{}
	if key.Kind == "service" {
		org, err := s.readyOrganization(ctx, key.ServiceOrganization)
		return []Organization{org}, err
	}
	items, err := s.config.Provider.Memberships(ctx, key.Owner, "")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.UserID != key.Owner || item.Status != "active" {
			continue
		}
		org, err := s.registry.ByProvider(ctx, item.OrganizationID)
		if errors.Is(err, ErrOrganizationNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if org.State != "ready" {
			continue
		}
		if _, err := s.keyTenantCall(ctx, key, org, "/internal/v1/keys/catalog", struct{}{}); err != nil {
			var refusal *apikey.Refusal
			if errors.As(err, &refusal) {
				continue
			}
			return nil, err
		}
		if !slices.ContainsFunc(organizations, func(existing Organization) bool { return existing.ID == org.ID }) {
			organizations = append(organizations, org)
		}
	}
	slices.SortFunc(organizations, func(a, b Organization) int { return strings.Compare(a.ID, b.ID) })
	return organizations, nil
}

func (s *Service) keyTenantCall(ctx context.Context, key accessKey, org Organization, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	claims, err := s.keyClaims(ctx, key, org, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	if err := s.recordKeyUse(ctx, key.ID, org.ID); err != nil {
		return nil, err
	}
	status, raw, err := s.signedCall(ctx, org, claims, body, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		var refusal apikey.Refusal
		if json.Unmarshal(raw, &refusal) == nil && refusal.Code != "" {
			return nil, &refusal
		}
		return nil, &apikey.Refusal{Code: "tenant_unavailable", Message: "The target organization could not execute this call"}
	}
	return raw, nil
}

func (accessKeyExecutor) OrganizationSelectors() bool { return true }

func (e accessKeyExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	key, err := e.current(ctx)
	if err != nil {
		return nil, err
	}
	organizations, err := e.service.keyOrganizations(ctx, key)
	if err != nil {
		return nil, err
	}
	byName := map[string]operatortool.Definition{}
	for _, org := range organizations {
		raw, err := e.service.keyTenantCall(ctx, key, org, "/internal/v1/keys/catalog", struct{}{})
		if err != nil {
			var refusal *apikey.Refusal
			if errors.As(err, &refusal) && (refusal.Code == "membership_denied" || refusal.Code == "access_denied") {
				continue
			}
			return nil, err
		}
		var definitions []operatortool.Definition
		if err := json.Unmarshal(raw, &definitions); err != nil {
			return nil, err
		}
		for _, definition := range definitions {
			if definition.Name == operatortool.OrganizationList || definition.Name == operatortool.ConnectionInfo {
				continue
			}
			byName[definition.Name] = definition
		}
	}
	for _, name := range []string{operatortool.OrganizationList, operatortool.ConnectionInfo} {
		definition, _ := operatortool.Lookup(name)
		byName[name] = definition
	}
	result := make([]operatortool.Definition, 0, len(byName))
	for _, definition := range byName {
		result = append(result, definition)
	}
	slices.SortFunc(result, func(a, b operatortool.Definition) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

func (e accessKeyExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	key, err := e.current(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	if call.Name == operatortool.OrganizationList {
		var input operatoradmin.Input
		if err := operatortool.DecodeArguments(call.Arguments, &input); err != nil {
			return operatortool.Result{}, err
		}
		organizations, err := e.service.keyOrganizations(ctx, key)
		if err != nil {
			return operatortool.Result{}, err
		}
		type choice struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		choices := make([]choice, 0, len(organizations))
		for _, org := range organizations {
			choices = append(choices, choice{org.ID, org.Name})
		}
		return operatortool.EncodeResult(struct {
			Organizations operatoradmin.PageResult[choice] `json:"organizations"`
		}{operatoradmin.Page(choices, input)})
	}
	if call.Name == operatortool.ConnectionInfo {
		return operatortool.EncodeResult(struct {
			Key accessKey `json:"key"`
			URL string    `json:"url"`
		}{key, e.service.config.PublicURL + "/mcp"})
	}
	var args map[string]json.RawMessage
	if err := operatortool.DecodeArguments(call.Arguments, &args); err != nil {
		return operatortool.Result{}, err
	}
	var organization string
	if json.Unmarshal(args["organization_id"], &organization) != nil || !safeID(organization) {
		return operatortool.Result{}, &apikey.Refusal{Code: "organization_required", Message: "Select organization_id for this call"}
	}
	org, err := e.service.readyOrganization(ctx, organization)
	if err != nil {
		return operatortool.Result{}, &apikey.Refusal{Code: "key_organization_denied", Message: "The target organization is unavailable to this key"}
	}
	definition, found := operatortool.Lookup(call.Name)
	if !found {
		return operatortool.Result{}, operatortool.ErrUnknownTool
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		return operatortool.Result{}, err
	}
	if _, needsOrg := schema.Properties["organization_id"]; !needsOrg {
		delete(args, "organization_id")
	}
	arguments, err := json.Marshal(args)
	if err != nil {
		return operatortool.Result{}, err
	}
	connection := operatortool.CurrentConnection(ctx)
	raw, err := e.service.keyTenantCall(ctx, key, org, "/internal/v1/keys/call", struct {
		Name         string          `json:"name"`
		Arguments    json.RawMessage `json:"arguments"`
		ConnectionID string          `json:"connection_id"`
	}{call.Name, arguments, key.ID + ":" + connection.ID})
	return operatortool.Result{Content: raw}, err
}

func (s *Service) keyOrganizationsJSON(c echo.Context) error {
	result, err := (accessKeyExecutor{s}).Execute(c.Request().Context(), operatortool.Call{Name: operatortool.OrganizationList, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		return keyError(c, err)
	}
	return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, result.Content)
}

func (s *Service) keyAPI(c echo.Context) error {
	organization := c.Request().Header.Get("X-Detent-Organization")
	if !safeID(organization) {
		return keyError(c, &apikey.Refusal{Code: "organization_required", Message: "Select X-Detent-Organization for this call"})
	}
	request := c.Request()
	request.URL.Path = "/api/v2/organizations/" + organization + strings.TrimPrefix(request.URL.Path, "/api/v2")
	request.URL.RawPath = ""
	c.SetParamNames("organization")
	c.SetParamValues(organization)
	return s.proxy(c)
}
