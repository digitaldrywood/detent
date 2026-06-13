package web

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Server) reports(c echo.Context) error {
	from, to, response, status := reportsDateRange(c)
	if response != nil {
		return c.JSON(status, response)
	}

	ctx := c.Request().Context()
	data, err := s.reportsData(ctx, from, to)
	if err != nil {
		s.logger.Error("usage reports page failed", slog.Any("error", err))
		return c.JSON(http.StatusInternalServerError, errorResponse("usage_reports_failed", "Usage reports failed"))
	}
	data.SidebarCollapsed = dashboardSidebarCollapsed(c.Request())

	return render(c, templates.Reports(data))
}

func reportsDateRange(c echo.Context) (time.Time, time.Time, *apiErrorResponse, int) {
	from, response, status := usageDate("from", c.QueryParam("from"))
	if response != nil {
		return time.Time{}, time.Time{}, response, status
	}
	to, response, status := usageDate("to", c.QueryParam("to"))
	if response != nil {
		return time.Time{}, time.Time{}, response, status
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		response := errorResponse("invalid_date_range", "from must be on or before to")
		return time.Time{}, time.Time{}, &response, http.StatusBadRequest
	}
	return from, to, nil, 0
}

func (s *Server) reportsData(ctx context.Context, from time.Time, to time.Time) (templates.ReportsData, error) {
	day, err := s.usageReportData(ctx, store.UsageReportByDay, from, to)
	if err != nil {
		return templates.ReportsData{}, err
	}
	project, err := s.usageReportData(ctx, store.UsageReportByProject, from, to)
	if err != nil {
		return templates.ReportsData{}, err
	}
	issue, err := s.usageReportData(ctx, store.UsageReportByIssue, from, to)
	if err != nil {
		return templates.ReportsData{}, err
	}
	pr, err := s.usageReportData(ctx, store.UsageReportByPR, from, to)
	if err != nil {
		return templates.ReportsData{}, err
	}
	model, err := s.usageReportData(ctx, store.UsageReportByModel, from, to)
	if err != nil {
		return templates.ReportsData{}, err
	}

	instanceName := s.instanceName()
	snapshot := s.latestSnapshot(ctx)
	return templates.ReportsData{
		Title:           instancePageTitle(instanceName, "Detent reports"),
		ApplicationName: applicationName(instanceName),
		InstanceName:    instanceName,
		ConnectorName:   s.connector.Name(),
		GeneratedAt:     time.Now().UTC().Truncate(time.Second),
		Day:             day,
		Project:         project,
		Issue:           issue,
		PR:              pr,
		Model:           model,
		Assets:          s.assets.templatePaths(),
		Projects:        s.projectSmallMultiples(ctx, snapshot),
		ActiveNav:       "reports",
	}, nil
}

func (s *Server) usageReportData(ctx context.Context, by store.UsageReportGroup, from time.Time, to time.Time) (templates.UsageReportData, error) {
	report, err := s.store.UsageReport(ctx, store.UsageReportQuery{
		By:   by,
		From: from,
		To:   to,
	})
	if err != nil {
		return templates.UsageReportData{}, err
	}
	return usageReportTemplateData(usageReportResponse(report, s.pricing)), nil
}

func usageReportTemplateData(response usageReportAPIResponse) templates.UsageReportData {
	return templates.UsageReportData{
		By:         response.By,
		From:       optionalStringValue(response.From),
		To:         optionalStringValue(response.To),
		Totals:     usageTotalsTemplateData(response.Totals),
		Series:     usageBucketTemplateData(response.Series),
		Breakdowns: usageBucketTemplateData(response.Breakdowns),
	}
}

func usageTotalsTemplateData(totals usageTotalsAPIResponse) templates.UsageTotalsData {
	return templates.UsageTotalsData{
		InputTokens:    totals.InputTokens,
		OutputTokens:   totals.OutputTokens,
		TotalTokens:    totals.TotalTokens,
		RuntimeSeconds: totals.RuntimeSeconds,
		Events:         totals.Events,
		SpendUSD:       totals.SpendUSD,
		Models:         usageModelsTemplateData(totals.Models),
	}
}

func usageBucketTemplateData(rows []usageBucketAPIResponse) []templates.UsageBucketData {
	payload := make([]templates.UsageBucketData, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, templates.UsageBucketData{
			Bucket:         row.Bucket,
			Label:          row.Label,
			Date:           optionalStringValue(row.Date),
			InputTokens:    row.InputTokens,
			OutputTokens:   row.OutputTokens,
			TotalTokens:    row.TotalTokens,
			RuntimeSeconds: row.RuntimeSeconds,
			Events:         row.Events,
			SpendUSD:       row.SpendUSD,
			Models:         usageModelsTemplateData(row.Models),
		})
	}
	return payload
}

func usageModelsTemplateData(models []usageModelAPIResponse) []templates.UsageModelData {
	payload := make([]templates.UsageModelData, 0, len(models))
	for _, model := range models {
		payload = append(payload, templates.UsageModelData{
			Model:          model.Model,
			InputTokens:    model.InputTokens,
			OutputTokens:   model.OutputTokens,
			TotalTokens:    model.TotalTokens,
			RuntimeSeconds: model.RuntimeSeconds,
			Events:         model.Events,
			SpendUSD:       model.SpendUSD,
		})
	}
	return payload
}

func optionalStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
