package cloudentry

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

type platformAuditRow struct {
	At                  string `json:"at"`
	Actor               string `json:"actor"`
	Event               string `json:"event"`
	OrganizationID      string `json:"organization_id"`
	OrganizationName    string `json:"organization_name"`
	OrganizationDeleted bool   `json:"organization_deleted"`
	Detail              string `json:"detail"`
	Source              string `json:"source"`
	ID                  string `json:"id"`
}

type platformAuditTenant struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

type platformAuditResult struct {
	Rows       []platformAuditRow    `json:"rows"`
	Events     []string              `json:"events"`
	Tenants    []platformAuditTenant `json:"tenants"`
	NextCursor string                `json:"next_cursor"`
}

type platformAuditCursor struct {
	At     string `json:"at"`
	Source string `json:"source"`
	ID     string `json:"id"`
}

type platformAuditFilter struct {
	Tenant, Actor, Event, From, To string
	Cursor                         platformAuditCursor
	Limit                          int
}

func auditTimeKey(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000")
}

func parseAuditFilter(query url.Values) (platformAuditFilter, error) {
	f := platformAuditFilter{Tenant: query.Get("tenant"), Actor: strings.TrimSpace(query.Get("actor")), Event: query.Get("event"), Limit: 50}
	invalid := errors.New("invalid audit filters")
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 200 {
			return f, invalid
		}
		f.Limit = limit
	}
	for _, field := range []struct {
		name string
		out  *string
	}{{"from", &f.From}, {"to", &f.To}} {
		value := query.Get(field.name)
		if value == "" {
			continue
		}
		at, err := time.Parse(time.DateOnly, value)
		if err != nil {
			at, err = parseTime(value)
		} else if field.name == "to" {
			at = at.AddDate(0, 0, 1).Add(-time.Nanosecond)
		}
		if err != nil {
			return f, invalid
		}
		*field.out = auditTimeKey(at)
	}
	if f.From != "" && f.To != "" && f.From > f.To {
		return f, invalid
	}
	if value := query.Get("cursor"); value != "" {
		data, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil || len(data) > 512 || json.Unmarshal(data, &f.Cursor) != nil {
			return f, invalid
		}
		at, err := time.Parse("2006-01-02T15:04:05.000000000", f.Cursor.At)
		if err != nil || auditTimeKey(at) != f.Cursor.At || f.Cursor.ID == "" || !slices.Contains([]string{"audit", "entitlement_changes", "organization_events", "platform_member_changes"}, f.Cursor.Source) {
			return f, invalid
		}
	}
	return f, nil
}

const (
	auditRecordedKey = "substr(replace(recorded_at,'Z','') || CASE WHEN instr(recorded_at,'.')=0 THEN '.' ELSE '' END || '000000000',1,29)"
	auditPagePrefix  = `WITH events AS (`
	auditPageSuffix  = `), keyed AS (SELECT *,` + auditRecordedKey + ` AS at FROM events)
SELECT id,recorded_at,organization_id,actor,event,detail FROM keyed
WHERE (?='' OR organization_id=?) AND (?='' OR instr(lower(actor),lower(?))>0)
AND (?='' OR event=?) AND (?='' OR at>=?) AND (?='' OR at<=?)
AND (?='' OR (at,?,id)<(?,?,?)) ORDER BY at DESC,id DESC LIMIT ?`
	authAuditEvents = `SELECT CAST(id AS TEXT) AS id,recorded_at,organization_id,
COALESCE((SELECT email FROM sessions WHERE subject=a.subject ORDER BY created_at DESC,token_hash DESC LIMIT 1),subject) AS actor,
CASE WHEN event LIKE 'support_requested:%' OR event LIKE 'support_started:%' THEN substr(event,1,instr(event,':')-1) ELSE event END AS event,
CASE WHEN event LIKE 'support_requested:%' OR event LIKE 'support_started:%' THEN substr(event,instr(event,':')+1) ELSE '' END AS detail FROM audit a WHERE NOT json_valid(event)`
	entitlementAuditEvents = `SELECT CAST(rowid AS TEXT) AS id,recorded_at,organization_id,staff_email AS actor,
CASE action WHEN 'grant' THEN 'plan_granted' ELSE 'plan_revoked' END AS event,
plan_id || ' v' || plan_version || CASE WHEN expires_at='' THEN '' ELSE ' until ' || expires_at END || ' · ' || reason AS detail FROM entitlement_changes`
	organizationAuditEvents = `SELECT CAST(id AS TEXT) AS id,recorded_at,organization_id,'system' AS actor,'organization_' || event AS event,'generation ' || generation AS detail FROM organization_events`
	memberAuditEvents       = `SELECT CAST(id AS TEXT) AS id,recorded_at,'' AS organization_id,actor_email AS actor,'member_' || action AS event,
email || CASE WHEN action='role_changed' THEN ' from ' || previous_role || ' to ' || role WHEN role!='' THEN ' as ' || role ELSE '' END || ' · ' || reason AS detail FROM platform_member_changes`
)

func readAuditRows(ctx context.Context, db *sql.DB, query, source string, f platformAuditFilter) ([]platformAuditRow, error) {
	rows, err := db.QueryContext(ctx, query, f.Tenant, f.Tenant, f.Actor, f.Actor, f.Event, f.Event, f.From, f.From, f.To, f.To,
		f.Cursor.At, source, f.Cursor.At, f.Cursor.Source, f.Cursor.ID, f.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []platformAuditRow{}
	for rows.Next() {
		row := platformAuditRow{Source: source}
		if err := rows.Scan(&row.ID, &row.At, &row.OrganizationID, &row.Actor, &row.Event, &row.Detail); err != nil {
			return nil, err
		}
		at, err := parseTime(row.At)
		if err != nil {
			return nil, err
		}
		row.At = auditTimeKey(at)
		result = append(result, row)
	}
	return result, rows.Err()
}

func readAuditEvents(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var event string
		if err := rows.Scan(&event); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Service) readPlatformAudit(ctx context.Context, f platformAuditFilter) (platformAuditResult, error) {
	result := platformAuditResult{Rows: []platformAuditRow{}, Events: []string{}, Tenants: []platformAuditTenant{}}
	sources := []struct {
		name                   string
		db                     *sql.DB
		pageQuery, eventsQuery string
	}{
		{"audit", s.auth.store.db, auditPagePrefix + authAuditEvents + auditPageSuffix, "SELECT DISTINCT event FROM (" + authAuditEvents + ")"},
		{"entitlement_changes", s.registry.store.db, auditPagePrefix + entitlementAuditEvents + auditPageSuffix, "SELECT DISTINCT event FROM (" + entitlementAuditEvents + ")"},
		{"organization_events", s.registry.store.db, auditPagePrefix + organizationAuditEvents + auditPageSuffix, "SELECT DISTINCT event FROM (" + organizationAuditEvents + ")"},
		{"platform_member_changes", s.registry.store.db, auditPagePrefix + memberAuditEvents + auditPageSuffix, "SELECT DISTINCT event FROM (" + memberAuditEvents + ")"},
	}
	events := map[string]bool{}
	for _, source := range sources {
		rows, err := readAuditRows(ctx, source.db, source.pageQuery, source.name, f)
		if err != nil {
			return result, err
		}
		result.Rows = append(result.Rows, rows...)
		names, err := readAuditEvents(ctx, source.db, source.eventsQuery)
		if err != nil {
			return result, err
		}
		for _, name := range names {
			events[name] = true
		}
	}
	slices.SortFunc(result.Rows, func(a, b platformAuditRow) int {
		if order := cmp.Compare(b.At, a.At); order != 0 {
			return order
		}
		if order := cmp.Compare(b.Source, a.Source); order != 0 {
			return order
		}
		return cmp.Compare(b.ID, a.ID)
	})
	if len(result.Rows) > f.Limit {
		result.Rows = result.Rows[:f.Limit]
		last := result.Rows[len(result.Rows)-1]
		data, err := json.Marshal(platformAuditCursor{At: last.At, Source: last.Source, ID: last.ID})
		if err != nil {
			return result, err
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	for index := range result.Rows {
		result.Rows[index].At += "Z"
	}
	for event := range events {
		result.Events = append(result.Events, event)
	}
	slices.Sort(result.Events)
	rows, err := s.registry.store.db.QueryContext(ctx, "SELECT id,name,state='deleted' FROM organizations ORDER BY name,id")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	tenants := map[string]platformAuditTenant{}
	for rows.Next() {
		var tenant platformAuditTenant
		if err := rows.Scan(&tenant.ID, &tenant.Name, &tenant.Deleted); err != nil {
			return result, err
		}
		tenants[tenant.ID] = tenant
		result.Tenants = append(result.Tenants, tenant)
	}
	for index, row := range result.Rows {
		if row.OrganizationID != "" {
			tenant, exists := tenants[row.OrganizationID]
			result.Rows[index].OrganizationDeleted = !exists || tenant.Deleted
			if exists && !tenant.Deleted {
				result.Rows[index].OrganizationName = tenant.Name
			}
		}
	}
	return result, rows.Err()
}

func (s *Service) platformAuditJSON(c echo.Context) error {
	if _, allowed, err := s.platformSession(c, "platform_audit_viewed"); !allowed {
		return err
	}
	filter, err := parseAuditFilter(c.QueryParams())
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "Choose valid dates, a page size from 1 to 200, and a valid audit cursor"})
	}
	result, err := s.readPlatformAudit(c.Request().Context(), filter)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "audit_unavailable", "message": "The audit timeline is temporarily unavailable"})
	}
	return c.JSON(http.StatusOK, result)
}
