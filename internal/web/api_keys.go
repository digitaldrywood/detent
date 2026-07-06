package web

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type apiKeyCreatePayload struct {
	Name       string   `json:"name" form:"name"`
	Scopes     []string `json:"scopes" form:"scopes"`
	ProjectIDs []string `json:"project_ids" form:"project_ids"`
	ExpiresIn  string   `json:"expires_in" form:"expires_in"`
}

type apiKeyRotatePayload struct {
	Grace string `json:"grace" form:"grace"`
}

type apiKeysListResponse struct {
	Keys []apiKeyResponse `json:"keys"`
}

type apiKeyCreatedResponse struct {
	Key   apiKeyResponse `json:"key"`
	Token string         `json:"token"`
}

type apiKeyResponse struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	PrefixLast4 string     `json:"prefix_last4"`
	Scopes      []string   `json:"scopes"`
	ProjectIDs  []string   `json:"project_ids"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

func (s *Server) apiKeysPage(c echo.Context) error {
	data, err := s.apiKeysData(c.Request().Context(), c)
	if err != nil {
		s.logger.Error("api keys page failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys page failed"))
	}
	applyAPIKeysPreferences(c.Request(), &data)
	return render(c, templates.APIKeysPage(data))
}

func (s *Server) apiKeysList(c echo.Context) error {
	data, err := s.apiKeysData(c.Request().Context(), c)
	if err != nil {
		s.logger.Error("api keys list failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys list failed"))
	}
	if htmxRequest(c) {
		return render(c, templates.APIKeysTable(data))
	}
	keys, err := s.store.ListAPIKeys(c.Request().Context())
	if err != nil {
		s.logger.Error("api keys json list failed", "error", err)
		return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys list failed"))
	}
	responses := make([]apiKeyResponse, 0, len(keys))
	now := apiNow()
	for _, key := range keys {
		responses = append(responses, apiKeyJSON(key, now))
	}
	return c.JSON(http.StatusOK, apiKeysListResponse{Keys: responses})
}

func (s *Server) apiKeysCreate(c echo.Context) error {
	payload, err := apiKeyCreateRequest(c)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_request", "Request body must be valid JSON or form data"))
	}
	created, err := s.apiKeys.Create(c.Request().Context(), apikey.CreateRequest{
		Name:       payload.Name,
		Scopes:     payload.Scopes,
		ProjectIDs: payload.ProjectIDs,
		ExpiresIn:  payload.ExpiresIn,
	})
	if err != nil {
		return apiKeyManagementError(c, err)
	}
	if htmxRequest(c) {
		data, dataErr := s.apiKeysData(c.Request().Context(), c)
		if dataErr != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys page failed"))
		}
		c.Response().Header().Set("HX-Trigger", "apiKeyCreated")
		return render(c, templates.APIKeyReveal(data, created.Token))
	}
	return c.JSON(http.StatusCreated, apiKeyCreatedResponse{
		Key:   apiKeyJSON(created.Key, apiNow()),
		Token: created.Token,
	})
}

func (s *Server) apiKeysRotateDialog(c echo.Context) error {
	data, err := s.apiKeysData(c.Request().Context(), c)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys page failed"))
	}
	row, ok := apiKeyRowByID(data, c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("api_key_not_found", "API key not found"))
	}
	return render(c, templates.APIKeyRotateDialog(data, row))
}

func (s *Server) apiKeysRotate(c echo.Context) error {
	payload, err := apiKeyRotateRequest(c)
	if err != nil {
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_request", "Request body must be valid JSON or form data"))
	}
	created, err := s.apiKeys.Rotate(c.Request().Context(), c.Param("id"), payload.Grace)
	if err != nil {
		return apiKeyManagementError(c, err)
	}
	if htmxRequest(c) {
		data, dataErr := s.apiKeysData(c.Request().Context(), c)
		if dataErr != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys page failed"))
		}
		c.Response().Header().Set("HX-Trigger", "apiKeyChanged")
		return render(c, templates.APIKeyReveal(data, created.Token))
	}
	return c.JSON(http.StatusCreated, apiKeyCreatedResponse{
		Key:   apiKeyJSON(created.Key, apiNow()),
		Token: created.Token,
	})
}

func (s *Server) apiKeysRevoke(c echo.Context) error {
	if err := s.apiKeys.Revoke(c.Request().Context(), c.Param("id")); err != nil {
		return apiKeyManagementError(c, err)
	}
	if htmxRequest(c) {
		data, dataErr := s.apiKeysData(c.Request().Context(), c)
		if dataErr != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse("api_keys_failed", "API keys page failed"))
		}
		c.Response().Header().Set("HX-Trigger", "apiKeyChanged")
		return render(c, templates.APIKeysTable(data))
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) apiKeysData(ctx context.Context, c echo.Context) (templates.APIKeysData, error) {
	instanceName := s.instanceName()
	snapshot := s.latestSnapshot(ctx)
	sidebarProjects := s.projectSmallMultiples(ctx, snapshot)
	keys, err := s.store.ListAPIKeys(ctx)
	if err != nil {
		return templates.APIKeysData{}, err
	}
	search := strings.TrimSpace(c.QueryParam("q"))
	sortKey := strings.TrimSpace(c.QueryParam("sort"))
	if sortKey == "" {
		sortKey = "created"
	}
	activeRows, inactiveRows := apiKeyRows(keys, apiNow(), search, sortKey)
	exampleProject := apiKeyExampleProject(s.registry)
	return templates.APIKeysData{
		Title:                   instancePageTitle(instanceName, "Detent API Keys"),
		ApplicationName:         applicationName(instanceName),
		InstanceName:            instanceName,
		Version:                 s.version,
		Build:                   s.build,
		Snapshot:                snapshot,
		Assets:                  s.assets.templatePaths(),
		SidebarProjects:         sidebarProjects,
		ActiveNav:               "api-keys",
		Search:                  search,
		Sort:                    sortKey,
		ActiveRows:              activeRows,
		InactiveRows:            inactiveRows,
		ProjectOptions:          apiKeyProjectOptions(s.registry),
		ShowStaticOnlyBanner:    !serverAddressLoopback(s.serverAddr) && s.apiToken() != "" && len(keys) == 0,
		StaticTokenConfigured:   s.apiToken() != "",
		WorkItemExampleProject:  exampleProject,
		WorkItemExampleEndpoint: apiKeyExampleEndpoint(s.dashboardURL, exampleProject),
	}, nil
}

func apiKeyCreateRequest(c echo.Context) (apiKeyCreatePayload, error) {
	var payload apiKeyCreatePayload
	if strings.Contains(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		if err := c.Bind(&payload); err != nil {
			return payload, err
		}
		return payload, nil
	}
	form, err := c.FormParams()
	if err != nil {
		return payload, err
	}
	payload.Name = form.Get("name")
	payload.Scopes = form["scopes"]
	payload.ExpiresIn = form.Get("expires_in")
	if form.Get("all_projects") != "true" {
		payload.ProjectIDs = form["project_ids"]
	}
	return payload, nil
}

func apiKeyRotateRequest(c echo.Context) (apiKeyRotatePayload, error) {
	var payload apiKeyRotatePayload
	if strings.Contains(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		if err := c.Bind(&payload); err != nil {
			return payload, err
		}
		return payload, nil
	}
	payload.Grace = c.FormValue("grace")
	return payload, nil
}

func apiKeyManagementError(c echo.Context, err error) error {
	status := http.StatusUnprocessableEntity
	code := "invalid_request"
	message := err.Error()
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
		code = "api_key_not_found"
		message = "API key not found"
	case errors.Is(err, apikey.ErrKeyLimitExceeded):
		status = http.StatusConflict
		code = "api_key_limit_exceeded"
		message = "API key limit exceeded"
	case errors.Is(err, apikey.ErrNameRequired):
		code = "name_required"
		message = "Name is required"
	case errors.Is(err, apikey.ErrInvalidScope):
		code = "invalid_scope"
		message = "Scope must be read, write, or admin"
	case errors.Is(err, apikey.ErrInvalidExpiry):
		code = "invalid_expiry"
		message = "expires_in must be 30d, 90d, 365d, or never"
	case errors.Is(err, apikey.ErrInvalidGrace):
		code = "invalid_grace"
		message = "grace must be 1h, 24h, or 7d"
	case errors.Is(err, apikey.ErrKeyRevoked):
		code = "token_revoked"
		message = "API key has been revoked"
	}
	return c.JSON(status, errorResponse(code, message))
}

func apiKeyRows(keys []store.APIKey, now time.Time, search string, sortKey string) ([]templates.APIKeyRow, []templates.APIKeyRow) {
	activeRows := []templates.APIKeyRow{}
	inactiveRows := []templates.APIKeyRow{}
	search = strings.ToLower(strings.TrimSpace(search))
	for _, key := range keys {
		if search != "" && !strings.Contains(strings.ToLower(key.Name), search) {
			continue
		}
		row, active := apiKeyRow(key, now)
		if active {
			activeRows = append(activeRows, row)
		} else {
			inactiveRows = append(inactiveRows, row)
		}
	}
	sortRows := func(rows []templates.APIKeyRow) {
		sort.SliceStable(rows, func(i, j int) bool {
			switch sortKey {
			case "name":
				return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
			case "last-used":
				return rows[i].SortLastUsed > rows[j].SortLastUsed
			default:
				return rows[i].SortCreated > rows[j].SortCreated
			}
		})
	}
	sortRows(activeRows)
	sortRows(inactiveRows)
	return activeRows, inactiveRows
}

func apiKeyRow(key store.APIKey, now time.Time) (templates.APIKeyRow, bool) {
	scope := string(apikey.HighestScope(key.Scopes))
	status := "Active"
	statusClass := "text-ok"
	active := true
	if key.RevokedAt != nil {
		status = "Revoked"
		statusClass = "text-err"
		active = false
	} else if key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
		status = "Expired"
		statusClass = "text-err"
		active = false
	} else if key.ExpiresAt != nil {
		days := int(key.ExpiresAt.Sub(now).Hours() / 24)
		if days < 1 {
			status = "Expires today"
		} else {
			status = "Expires in " + strconv.FormatInt(int64(days), 10) + "d"
		}
		if days <= 7 {
			statusClass = "text-warn"
		}
	}
	lastUsed := "Never"
	lastUsedClass := "text-dim"
	sortLastUsed := ""
	unusedBadge := false
	if key.LastUsedAt != nil {
		lastUsed = relativeTime(*key.LastUsedAt, now)
		lastUsedClass = "text-sec"
		sortLastUsed = key.LastUsedAt.UTC().Format(time.RFC3339)
	} else if now.Sub(key.CreatedAt) > 90*24*time.Hour {
		unusedBadge = true
	}
	return templates.APIKeyRow{
		ID:               key.ID,
		Name:             key.Name,
		PrefixLast4:      key.PrefixLast4,
		Scope:            scope,
		ProjectLabels:    append([]string(nil), key.ProjectIDs...),
		Status:           status,
		StatusClass:      statusClass,
		LastUsed:         lastUsed,
		LastUsedClass:    lastUsedClass,
		UnusedBadge:      unusedBadge,
		Created:          key.CreatedAt.Format("Jan 2, 2006"),
		SortCreated:      key.CreatedAt.UTC().Format(time.RFC3339),
		SortLastUsed:     sortLastUsed,
		AdminConfirmName: apikey.HasScope(key.Scopes, apikey.ScopeAdmin),
	}, active
}

func apiKeyJSON(key store.APIKey, now time.Time) apiKeyResponse {
	row, _ := apiKeyRow(key, now)
	return apiKeyResponse{
		ID:          key.ID,
		Name:        key.Name,
		PrefixLast4: key.PrefixLast4,
		Scopes:      append([]string(nil), key.Scopes...),
		ProjectIDs:  append([]string(nil), key.ProjectIDs...),
		Status:      strings.ToLower(strings.Fields(row.Status)[0]),
		CreatedAt:   key.CreatedAt,
		ExpiresAt:   key.ExpiresAt,
		LastUsedAt:  key.LastUsedAt,
		RevokedAt:   key.RevokedAt,
	}
}

func apiKeyRowByID(data templates.APIKeysData, id string) (templates.APIKeyRow, bool) {
	for _, row := range append(data.ActiveRows, data.InactiveRows...) {
		if row.ID == id {
			return row, true
		}
	}
	return templates.APIKeyRow{}, false
}

func apiKeyProjectOptions(registry *project.Registry) []templates.APIKeyProjectOption {
	if registry == nil {
		return nil
	}
	projects := registry.List()
	options := make([]templates.APIKeyProjectOption, 0, len(projects))
	for _, trackedProject := range projects {
		if trackedProject == nil {
			continue
		}
		id := string(trackedProject.ID())
		options = append(options, templates.APIKeyProjectOption{ID: id, Name: id})
	}
	return options
}

func apiKeyExampleProject(registry *project.Registry) string {
	options := apiKeyProjectOptions(registry)
	if len(options) == 0 {
		return "PROJECT_ID"
	}
	return options[0].ID
}

func apiKeyExampleEndpoint(baseURL string, projectID string) string {
	path := "/api/v1/projects/" + strings.TrimSpace(projectID) + "/work-items"
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return path
	}
	return baseURL + path
}

func relativeTime(value time.Time, now time.Time) string {
	if value.After(now) {
		return "just now"
	}
	d := now.Sub(value)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m ago"
	case d < 24*time.Hour:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h ago"
	default:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d ago"
	}
}
