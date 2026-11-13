package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsPagingKey struct{}
type analyticsPaging struct {
	name            string
	populationToken string
	offset          int
	issuesOffset    int
	next            map[string]string
}

func analyticsPagingContext(ctx context.Context, cursor string) (context.Context, *analyticsPaging, error) {
	if page := analyticsPagingFromContext(ctx); page != nil {
		return ctx, page, nil
	}
	page := &analyticsPaging{next: map[string]string{}}
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		switch parts[0] {
		case "attempts", "landings", "usage", "decisions", "capacity", "residence_issues", "residence_events", "failures", "quality_landings", "quality_events", "quality_evidence", "quality_worked":
		default:
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		if strings.Contains(parts[1], ".") && parts[2] == "0" {
			if len(cursor) > 4096 {
				return ctx, nil, errors.New("invalid analytics population cursor")
			}
			page.name, page.populationToken = parts[0], parts[1]
			return context.WithValue(ctx, analyticsPagingKey{}, page), page, nil
		}
		offset, err := strconv.Atoi(parts[1])
		issues, issueErr := strconv.Atoi(parts[2])
		if err != nil || issueErr != nil || offset < 0 || offset > int(^uint(0)>>1)-maxAnalyticsPopulation || issues < 0 || issues > int(^uint(0)>>1)-maxAnalyticsPopulation {
			return ctx, nil, errors.New("invalid analytics population cursor")
		}
		page.name, page.offset, page.issuesOffset = parts[0], offset, issues
	}
	return context.WithValue(ctx, analyticsPagingKey{}, page), page, nil
}

func analyticsPagingFromContext(ctx context.Context) *analyticsPaging {
	if page, ok := ctx.Value(analyticsPagingKey{}).(*analyticsPaging); ok {
		return page
	}
	return nil
}

func analyticsPopulationOffset(ctx context.Context, name string) int {
	page := analyticsPagingFromContext(ctx)
	if page == nil {
		return 0
	}
	if page.name == name {
		return page.offset
	}
	if name == "residence_issues" && page.name == "residence_events" {
		return page.issuesOffset
	}
	return 0
}

func (s *Service) analyticsAttemptCursor(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, runner, population string) (reportstore.AnalyticsAttemptCursor, nativeCursor, []byte, error) {
	if population != "attempts" && population != "quality_worked" {
		return reportstore.AnalyticsAttemptCursor{}, nativeCursor{}, nil, errors.New("invalid analytics attempt population")
	}
	if s.config.SecretKeys == nil {
		if page := analyticsPagingFromContext(ctx); page != nil && page.name == population && (page.populationToken != "" || page.offset != 0) {
			return reportstore.AnalyticsAttemptCursor{}, nativeCursor{}, nil, errors.New("analytics continuation requires configured secret encryption keys")
		}
		return reportstore.AnalyticsAttemptCursor{}, nativeCursor{}, nil, nil
	}
	params := url.Values{"organization": {string(scope.organization)}, "project": {string(scope.project)}, "from": {w.From.UTC().Format(time.RFC3339Nano)}, "to": {w.To.UTC().Format(time.RFC3339Nano)}, "runner": {runner}}
	bindAnalyticsCohort(params, ctx)
	if page := analyticsPagingFromContext(ctx); page != nil && page.name == population {
		if page.offset != 0 {
			return reportstore.AnalyticsAttemptCursor{}, nativeCursor{}, nil, errors.New("analytics attempt cursor requires a current seek key")
		}
		params.Set("cursor", page.populationToken)
	}
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, "operator/analytics/"+population, params, reportstore.AnalyticsAttemptLimit, reportstore.AnalyticsAttemptLimit)
	if err != nil {
		return reportstore.AnalyticsAttemptCursor{}, cursor, key, err
	}
	var after reportstore.AnalyticsAttemptCursor
	if cursor.After != "" {
		var envelope hubsecrets.Envelope
		if json.Unmarshal([]byte(cursor.After), &envelope) != nil {
			return after, cursor, key, errors.New("invalid analytics attempt cursor envelope")
		}
		raw, err := s.config.SecretKeys.Open(envelope, []byte(cursor.Scope))
		if err != nil {
			return after, cursor, key, err
		}
		defer clear(raw)
		if json.Unmarshal(raw, &after) != nil || after.ID == "" || after.StartedAt.IsZero() || after.Submicro < 0 || after.Submicro > 999 || after.StartedAt.Nanosecond()%1000 != 0 {
			return after, cursor, key, errors.New("invalid analytics attempt seek key")
		}
	}
	return after, cursor, key, nil
}

func (s *Service) encodeAnalyticsAttemptCursor(cursor nativeCursor, key []byte, after reportstore.AnalyticsAttemptCursor, population string) (string, error) {
	raw, err := json.Marshal(after)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	envelope, err := s.config.SecretKeys.Seal(raw, []byte(cursor.Scope))
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	cursor.After = string(encoded)
	token, err := encodeNativeCursor(cursor, key)
	if err != nil {
		return "", err
	}
	result := population + ":" + token + ":0"
	if len(result) > 4096 {
		return "", errors.New("analytics attempt cursor exceeds its bound")
	}
	return result, nil
}

func (s *Service) analyticsUsageCursor(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow) (reportstore.AnalyticsUsageCursor, nativeCursor, []byte, error) {
	if s.config.SecretKeys == nil {
		if page := analyticsPagingFromContext(ctx); page != nil && page.name == "usage" && (page.populationToken != "" || page.offset != 0) {
			return reportstore.AnalyticsUsageCursor{}, nativeCursor{}, nil, errors.New("analytics continuation requires configured secret encryption keys")
		}
		return reportstore.AnalyticsUsageCursor{}, nativeCursor{}, nil, nil
	}
	params := url.Values{"organization": {string(scope.organization)}, "project": {string(scope.project)}, "from": {w.From.UTC().Format(time.RFC3339Nano)}, "to": {w.To.UTC().Format(time.RFC3339Nano)}}
	bindAnalyticsCohort(params, ctx)
	if page := analyticsPagingFromContext(ctx); page != nil && page.name == "usage" {
		if page.offset != 0 {
			return reportstore.AnalyticsUsageCursor{}, nativeCursor{}, nil, errors.New("analytics usage cursor requires a current seek key")
		}
		params.Set("cursor", page.populationToken)
	}
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, "operator/analytics/usage", params, reportstore.AnalyticsAttemptLimit, reportstore.AnalyticsAttemptLimit)
	if err != nil {
		return reportstore.AnalyticsUsageCursor{}, cursor, key, err
	}
	var after reportstore.AnalyticsUsageCursor
	if cursor.After != "" {
		var envelope hubsecrets.Envelope
		if json.Unmarshal([]byte(cursor.After), &envelope) != nil {
			return after, cursor, key, errors.New("invalid analytics usage cursor envelope")
		}
		raw, err := s.config.SecretKeys.Open(envelope, []byte(cursor.Scope))
		if err != nil {
			return after, cursor, key, err
		}
		defer clear(raw)
		if json.Unmarshal(raw, &after) != nil || after.Period.IsZero() || after.AttemptID == "" || after.Source < 0 || after.Source > 1 || after.Source == 0 && (after.SourceKey == "" || after.Origin != "") || after.Source == 1 && after.Origin != "" && after.Origin != "import" && after.Origin != "current" || after.Source == 1 && after.SourceKey != "" && (len(after.SourceKey) != 3 || strings.Trim(strings.ToUpper(after.SourceKey), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "") {
			return after, cursor, key, errors.New("invalid analytics usage seek key")
		}
	}
	return after, cursor, key, nil
}

func (s *Service) encodeAnalyticsUsageCursor(cursor nativeCursor, key []byte, after reportstore.AnalyticsUsageCursor) (string, error) {
	raw, err := json.Marshal(after)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	envelope, err := s.config.SecretKeys.Seal(raw, []byte(cursor.Scope))
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	cursor.After = string(encoded)
	token, err := encodeNativeCursor(cursor, key)
	if err != nil {
		return "", err
	}
	result := "usage:" + token + ":0"
	if len(result) > 4096 {
		return "", errors.New("analytics usage cursor exceeds its bound")
	}
	return result, nil
}

func (s *Service) analyticsSchedulerCursor(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, name string) (eventstore.AnalyticsSchedulerCursor, nativeCursor, []byte, error) {
	if name != "decisions" && name != "capacity" {
		return eventstore.AnalyticsSchedulerCursor{}, nativeCursor{}, nil, errors.New("invalid scheduler analytics population")
	}
	if s.config.SecretKeys == nil {
		if page := analyticsPagingFromContext(ctx); page != nil && page.name == name && (page.populationToken != "" || page.offset != 0) {
			return eventstore.AnalyticsSchedulerCursor{}, nativeCursor{}, nil, errors.New("analytics continuation requires configured secret encryption keys")
		}
		return eventstore.AnalyticsSchedulerCursor{}, nativeCursor{}, nil, nil
	}
	params := url.Values{"organization": {string(scope.organization)}, "project": {string(scope.project)}, "from": {w.From.UTC().Format(time.RFC3339Nano)}, "to": {w.To.UTC().Format(time.RFC3339Nano)}}
	bindAnalyticsCohort(params, ctx)
	if page := analyticsPagingFromContext(ctx); page != nil && page.name == name {
		if page.offset != 0 {
			return eventstore.AnalyticsSchedulerCursor{}, nativeCursor{}, nil, errors.New("scheduler analytics cursor requires a current seek key")
		}
		params.Set("cursor", page.populationToken)
	}
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, "operator/analytics/"+name, params, maxAnalyticsPopulation, maxAnalyticsPopulation)
	if err != nil {
		return eventstore.AnalyticsSchedulerCursor{}, cursor, key, err
	}
	var after eventstore.AnalyticsSchedulerCursor
	if cursor.After != "" {
		var envelope hubsecrets.Envelope
		if json.Unmarshal([]byte(cursor.After), &envelope) != nil {
			return after, cursor, key, errors.New("invalid scheduler analytics cursor envelope")
		}
		raw, err := s.config.SecretKeys.Open(envelope, []byte(cursor.Scope))
		if err != nil {
			return after, cursor, key, err
		}
		defer clear(raw)
		if json.Unmarshal(raw, &after) != nil || after.At.IsZero() || after.ID == "" {
			return after, cursor, key, errors.New("invalid scheduler analytics seek key")
		}
	}
	return after, cursor, key, nil
}

func (s *Service) encodeAnalyticsSchedulerCursor(cursor nativeCursor, key []byte, after eventstore.AnalyticsSchedulerCursor, name string) (string, error) {
	raw, err := json.Marshal(after)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	envelope, err := s.config.SecretKeys.Seal(raw, []byte(cursor.Scope))
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	cursor.After = string(encoded)
	token, err := encodeNativeCursor(cursor, key)
	if err != nil {
		return "", err
	}
	result := name + ":" + token + ":0"
	if len(result) > 4096 {
		return "", errors.New("scheduler analytics cursor exceeds its bound")
	}
	return result, nil
}

type analyticsResidenceCursor struct {
	IssuesAfter   tracker.NativeWorkItemID
	EventItem     tracker.NativeWorkItemID
	EventSequence int64
}

func (s *Service) analyticsResidenceSeek(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, live bool) (analyticsResidenceCursor, nativeCursor, []byte, error) {
	after := analyticsResidenceCursor{}
	if s.config.SecretKeys == nil {
		if page := analyticsPagingFromContext(ctx); page != nil && (page.name == "residence_issues" || page.name == "residence_events") && (page.populationToken != "" || page.offset != 0) {
			return after, nativeCursor{}, nil, errors.New("analytics continuation requires configured secret encryption keys")
		}
		return after, nativeCursor{}, nil, nil
	}
	params := url.Values{"organization": {string(scope.organization)}, "project": {string(scope.project)}, "from": {w.From.UTC().Format(time.RFC3339Nano)}, "to": {w.To.UTC().Format(time.RFC3339Nano)}, "live": {strconv.FormatBool(live)}}
	bindAnalyticsCohort(params, ctx)
	if page := analyticsPagingFromContext(ctx); page != nil && (page.name == "residence_issues" || page.name == "residence_events") {
		if page.offset != 0 {
			return after, nativeCursor{}, nil, errors.New("residence analytics cursor requires a current seek key")
		}
		params.Set("cursor", page.populationToken)
	}
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, "operator/analytics/residence", params, maxAnalyticsPopulation, maxAnalyticsPopulation)
	if err != nil {
		return after, cursor, key, err
	}
	if cursor.After != "" {
		var envelope hubsecrets.Envelope
		if json.Unmarshal([]byte(cursor.After), &envelope) != nil {
			return after, cursor, key, errors.New("invalid residence analytics cursor envelope")
		}
		raw, err := s.config.SecretKeys.Open(envelope, []byte(cursor.Scope))
		if err != nil {
			return after, cursor, key, err
		}
		defer clear(raw)
		if json.Unmarshal(raw, &after) != nil || after.EventSequence < 0 || (after.EventItem == "") != (after.EventSequence == 0) {
			return after, cursor, key, errors.New("invalid residence analytics seek key")
		}
		if page := analyticsPagingFromContext(ctx); page.name == "residence_issues" && after.EventItem != "" || page.name == "residence_events" && after.EventItem == "" {
			return after, cursor, key, errors.New("invalid residence analytics population key")
		}
	}
	return after, cursor, key, nil
}

func (s *Service) encodeAnalyticsResidenceCursor(cursor nativeCursor, key []byte, after analyticsResidenceCursor, name string) (string, error) {
	raw, err := json.Marshal(after)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	envelope, err := s.config.SecretKeys.Seal(raw, []byte(cursor.Scope))
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	cursor.After = string(encoded)
	token, err := encodeNativeCursor(cursor, key)
	if err != nil {
		return "", err
	}
	result := name + ":" + token + ":0"
	if len(result) > 4096 {
		return "", errors.New("residence analytics cursor exceeds its bound")
	}
	return result, nil
}

type analyticsFactCursor struct {
	Failure  diagnosticstore.AnalyticsFailureCursor
	Landing  eventstore.AnalyticsLandingCursor
	Workflow eventstore.AnalyticsWorkflowCursor
	Evidence eventstore.AnalyticsEvidenceCursor
}

func (s *Service) analyticsFactSeek(ctx context.Context, scope nativeScope, w operatortool.AnalyticsWindow, name string) (analyticsFactCursor, nativeCursor, []byte, error) {
	after := analyticsFactCursor{}
	switch name {
	case "landings", "quality_landings", "quality_events", "quality_evidence", "failures":
	default:
		return after, nativeCursor{}, nil, errors.New("invalid analytics fact population")
	}
	if s.config.SecretKeys == nil {
		if page := analyticsPagingFromContext(ctx); page != nil && page.name == name && (page.populationToken != "" || page.offset != 0) {
			return after, nativeCursor{}, nil, errors.New("analytics continuation requires configured secret encryption keys")
		}
		return after, nativeCursor{}, nil, nil
	}
	params := url.Values{"organization": {string(scope.organization)}, "project": {string(scope.project)}, "from": {w.From.UTC().Format(time.RFC3339Nano)}, "to": {w.To.UTC().Format(time.RFC3339Nano)}}
	bindAnalyticsCohort(params, ctx)
	if page := analyticsPagingFromContext(ctx); page != nil && page.name == name {
		if page.offset != 0 {
			return after, nativeCursor{}, nil, errors.New("analytics fact cursor requires a current seek key")
		}
		params.Set("cursor", page.populationToken)
	}
	_, cursor, key, err := s.readNativePageLimit(ctx, scope, "operator/analytics/"+name, params, maxAnalyticsPopulation, maxAnalyticsPopulation)
	if err != nil {
		return after, cursor, key, err
	}
	if cursor.After != "" {
		var envelope hubsecrets.Envelope
		if json.Unmarshal([]byte(cursor.After), &envelope) != nil {
			return after, cursor, key, errors.New("invalid analytics fact cursor envelope")
		}
		raw, err := s.config.SecretKeys.Open(envelope, []byte(cursor.Scope))
		if err != nil {
			return after, cursor, key, err
		}
		defer clear(raw)
		if json.Unmarshal(raw, &after) != nil {
			return after, cursor, key, errors.New("invalid analytics fact seek key")
		}
	}
	return after, cursor, key, nil
}

func (s *Service) encodeAnalyticsFactCursor(cursor nativeCursor, key []byte, after analyticsFactCursor, name string) (string, error) {
	raw, err := json.Marshal(after)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	envelope, err := s.config.SecretKeys.Seal(raw, []byte(cursor.Scope))
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	cursor.After = string(encoded)
	token, err := encodeNativeCursor(cursor, key)
	if err != nil {
		return "", err
	}
	result := name + ":" + token + ":0"
	if len(result) > 4096 {
		return "", errors.New("analytics fact cursor exceeds its bound")
	}
	return result, nil
}
