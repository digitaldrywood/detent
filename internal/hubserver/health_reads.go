package hubserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type healthFindingsPage struct {
	ChangeSequence      int64                       `json:"change_sequence"`
	BaselineUnavailable []healthBaselineUnavailable `json:"baseline_unavailable"`
	Items               []healthFinding             `json:"items"`
	NextCursor          string                      `json:"next_cursor,omitempty"`
	LastTickAt          *time.Time                  `json:"last_tick_at"`
}

func (s *Service) getHealthFindings(c echo.Context) error {
	limit := 100
	if raw := c.QueryParam("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return s.nativeAPIError(c, nativePublicInvalid("limit must be between 1 and 100"))
		}
		limit = value
	}
	page, err := s.readHealthFindings(c.Request().Context(), nativeRequestScope(c), c.QueryParam("state"), c.QueryParam("since"), c.QueryParam("cursor"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readHealthFindings(ctx context.Context, scope nativeScope, state, since, cursor string, limit int) (healthFindingsPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	page := healthFindingsPage{Items: []healthFinding{}, BaselineUnavailable: []healthBaselineUnavailable{}}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "resolved" && state != "all" {
		return page, nativePublicInvalid("state must be open, resolved or all")
	}
	if limit < 1 || limit > 100 {
		return page, nativePublicInvalid("limit must be between 1 and 100")
	}
	var sinceAt *time.Time
	if since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return page, nativePublicInvalid("since must be an RFC3339 timestamp")
		}
		sinceAt = &at
	}
	after := int64(0)
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return page, nativePublicInvalid("invalid findings cursor")
		}
		after, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil || after < 1 {
			return page, nativePublicInvalid("invalid findings cursor")
		}
	}
	if s.config.TenantRepository == nil {
		return page, workItemStorageUnavailable()
	}
	reporter := ""
	if scope.credential.HostedRole == "reporter" {
		reporter = scope.actor().PrincipalID
		if reporter == "" {
			return page, corestore.ErrNotFound
		}
	}
	var records healthstore.Page
	projected := true
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.Reader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		records, err = owner.HealthFindings(ctx, scope.project, state, sinceAt, after, reporter, limit+1)
		if errors.Is(err, healthstore.ErrPagePending) {
			fallback, ok := r.(healthstore.FallbackReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			projected = false
			records, err = fallback.LegacyHealthFindings(ctx, scope.project, state, sinceAt, after, reporter, limit+1)
		}
		return err
	})
	if err != nil {
		return page, err
	}
	page.ChangeSequence = records.ChangeSequence
	if records.NextSequence > 0 {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(records.NextSequence, 10)))
	}
	for i, row := range records.Findings {
		if i == limit {
			page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(records.Findings[i-1].Sequence, 10)))
			break
		}
		f := healthFinding{ID: row.ID, Fingerprint: row.Fingerprint, Signal: row.Signal, Class: row.Class, Subject: healthSubject{Kind: row.SubjectKind, ID: row.SubjectID}, OpenedAt: row.OpenedAt, LastSeenAt: row.LastSeenAt, ResolvedAt: row.ResolvedAt, Severity: row.Severity, Summary: row.Summary, NextAction: row.NextAction, EmailUnavailable: row.EmailUnavailable}
		f.Evidence = row.DisplayEvidence
		if !projected && row.Evidence.Key != "" {
			raw, err := s.runDetailStore().ReadDetail(ctx, string(scope.organization), row.Evidence)
			if err != nil {
				return page, err
			}
			if err := json.Unmarshal(raw, &f.Evidence); err != nil {
				return page, err
			}
		}
		f.Evidence = scopeHealthEvidence(f.Evidence, scope.project)
		for _, link := range row.Issues {
			f.Issues = append(f.Issues, healthFindingIssue{ProjectID: link.ProjectID, WorkItemID: link.WorkItemID})
		}
		if len(f.Issues) > 0 {
			f.FiledBy = "health_detector"
		}
		if row.SlackUnavailableAt != nil {
			f.SlackUnavailable = &healthSlackFailure{Code: "slack_unavailable", StatusCode: row.SlackStatusCode, At: *row.SlackUnavailableAt}
		}
		page.Items = append(page.Items, f)
	}
	for _, gap := range records.Baselines {
		page.BaselineUnavailable = append(page.BaselineUnavailable, healthBaselineUnavailable{Project: string(gap.ProjectID), Signal: gap.Signal, Lane: gap.Lane})
	}
	page.LastTickAt = records.LastTickAt
	return page, nil
}
