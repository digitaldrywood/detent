package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type diagnosticsCoverage struct {
	Source   string `json:"source"`
	Observed *int   `json:"observed"`
	Total    *int   `json:"total"`
}

type diagnosticsCapacity struct {
	Hour  time.Time `json:"hour"`
	Slots float64   `json:"slots"`
	Todo  *int      `json:"todo"`
}

type diagnosticsReport struct {
	Requests      *requestMetricsReport      `json:"requests,omitempty"`
	From          time.Time                  `json:"from"`
	To            time.Time                  `json:"to"`
	Partial       bool                       `json:"partial"`
	Findings      any                        `json:"findings"`
	DetectorTick  *time.Time                 `json:"detector_tick"`
	Capacity      []diagnosticsCapacity      `json:"capacity"`
	Claimed       *int                       `json:"claimed"`
	Ready         *int                       `json:"ready"`
	Skipped       *int                       `json:"skipped"`
	SkipReasons   []nativeAnalyticsSkip      `json:"skip_reasons"`
	MergeEntered  int                        `json:"merge_entered"`
	MergeLanded   int                        `json:"merge_landed"`
	MergeWaitP50  *float64                   `json:"merge_wait_p50"`
	MergeWaitP90  *float64                   `json:"merge_wait_p90"`
	MergeWaitMax  *float64                   `json:"merge_wait_max"`
	Refused       []diagnosticsRefusal       `json:"refused"`
	Coverage      []diagnosticsCoverage      `json:"coverage"`
	Configuration []diagnosticsConfiguration `json:"configuration"`
}

type diagnosticsRefusal struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type diagnosticsConfiguration struct {
	Name   string  `json:"name"`
	Value  *string `json:"value"`
	Effect *string `json:"effect"`
}

func (s *Service) hostedDiagnostics(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.usageCredentialError(c, err)
	}
	window, err := usageRangeWindow(c.QueryParam("range"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	w := operatortool.AnalyticsWindow{From: window.From, To: window.To, Bucket: time.Hour}
	report, err := s.readCloudDiagnostics(ctx, credential, w)
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Diagnostics are temporarily unavailable")
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) readCloudDiagnostics(ctx context.Context, credential apiCredential, w operatortool.AnalyticsWindow) (diagnosticsReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return diagnosticsReport{}, workItemStorageUnavailable()
	}
	return s.readCloudDiagnosticsTenant(ctx, credential, w)
}

func (s *Service) diagnosticsRequestMetrics(ctx context.Context) (*requestMetricsReport, error) {
	requests, visible, err := s.adminRequestMetrics(ctx)
	if err != nil || !visible {
		return nil, err
	}
	return &requests, nil
}

func diagnosticsTodoDepth(issues []analyticsResidenceIssue, states map[string]tracker.NativeState, clipped bool, at time.Time) *int {
	if clipped {
		return nil
	}
	count := 0
	for _, issue := range issues {
		if !issue.created.Before(at) {
			continue
		}
		past := issue
		past.events = nil
		for _, event := range issue.events {
			if event.at.Before(at) {
				past.events = append(past.events, event)
			}
		}
		timeline := buildAnalyticsResidenceTimeline(past, states, at)
		if timeline.issue.partial {
			return nil
		}
		if timeline.state == "Todo" {
			count++
		}
	}
	return &count
}
