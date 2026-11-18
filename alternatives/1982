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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("limit must be between 1 and 100"))
		}
		limit = value
	}
	page, err := s.readHealthFindings(c.Request().Context(), hubscope.NativeRequestScope(c), c.QueryParam("state"), c.QueryParam("since"), c.QueryParam("cursor"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readHealthFindings(ctx context.Context, scope hubscope.NativeScope, state, since, cursor string, limit int) (healthFindingsPage, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	page := healthFindingsPage{Items: []healthFinding{}, BaselineUnavailable: []healthBaselineUnavailable{}}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "resolved" && state != "all" {
		return page, hubapi.NativePublicInvalid("state must be open, resolved or all")
	}
	if limit < 1 || limit > 100 {
		return page, hubapi.NativePublicInvalid("limit must be between 1 and 100")
	}
	var sinceAt *time.Time
	if since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return page, hubapi.NativePublicInvalid("since must be an RFC3339 timestamp")
		}
		sinceAt = &at
	}
	after := int64(0)
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return page, hubapi.NativePublicInvalid("invalid findings cursor")
		}
		after, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil || after < 1 {
			return page, hubapi.NativePublicInvalid("invalid findings cursor")
		}
	}
	if s.config.TenantRepository == nil {
		return page, hubapi.NativeWorkItemStorageUnavailable()
	}
	reporter := ""
	if scope.Credential.HostedRole == "reporter" {
		reporter = scope.Actor().PrincipalID
		if reporter == "" {
			return page, corestore.ErrNotFound
		}
	}
	var records healthstore.Page
	projected := true
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return page, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.Reader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		records, err = owner.HealthFindings(ctx, scope.Project, state, sinceAt, after, reporter, limit+1)
		if errors.Is(err, healthstore.ErrPagePending) {
			fallback, ok := r.(healthstore.FallbackReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			projected = false
			records, err = fallback.LegacyHealthFindings(ctx, scope.Project, state, sinceAt, after, reporter, limit+1)
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
			raw, err := s.runDetailStore().ReadDetail(ctx, string(scope.Organization), row.Evidence)
			if err != nil {
				return page, err
			}
			if err := json.Unmarshal(raw, &f.Evidence); err != nil {
				return page, err
			}
		}
		f.Evidence = scopeHealthEvidence(f.Evidence, scope.Project)
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
