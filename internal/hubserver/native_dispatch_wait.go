package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func dispatchNotificationKey(organization tracker.OrganizationID) string {
	return "dispatch:" + string(organization)
}

func credentialDispatchOrganizations(ctx context.Context, db nativeQueryer, id string) ([]tracker.OrganizationID, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT organization_id FROM token_grants WHERE token_id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var organizations []tracker.OrganizationID
	for rows.Next() {
		var organization tracker.OrganizationID
		if err := rows.Scan(&organization); err != nil {
			return nil, err
		}
		organizations = append(organizations, organization)
	}
	return organizations, rows.Err()
}

func runnerDispatchFingerprint(ctx context.Context, tx *sql.Tx, scope nativeScope, machine string, now time.Time) (string, error) {
	var routing, raw string
	err := tx.QueryRowContext(ctx, `SELECT json_array(m.capacity, m.version, r.state, r.capacity_limit, r.reported_capacity,
 r.os, r.architecture, r.tags_json, r.operations_json, r.routing_settings_json, r.problems_json,
 r.backend_isolation_json, r.reported_protocol_major, r.settings_rejected,
 julianday(m.last_heartbeat_at) > julianday(?), julianday(r.last_heartbeat_at) > julianday(?)), COALESCE(r.provider_reports_json, '[]')
 FROM machines m LEFT JOIN runner_identities r ON r.machine_id = m.id AND r.id = ?
 WHERE m.id = ?`, formatHubTime(now.Add(-runnerauth.HeartbeatTimeout)), formatHubTime(now.Add(-runnerauth.HeartbeatTimeout)), scope.credential.Runner.RunnerID, machine).Scan(&routing, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var reports []providercapacity.Report
	if err := json.Unmarshal([]byte(raw), &reports); err != nil {
		return "", err
	}
	for index := range reports {
		reports[index].Availability = reports[index].State(now)
		reports[index].ObservedAt = time.Time{}
	}
	providers, err := marshalNative(reports)
	return routing + providers, err
}

func (b *notificationBroker) dispatchCursor(key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	revision := b.revisions[key]
	b.revisions[key] = revision
	return fmt.Sprintf("%s:%d", b.epoch, revision)
}

func (s *Service) notifyNativeDispatch(ctx context.Context, scope nativeScope, input any) {
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Type != "run.finished" {
		return
	}
	s.notifications.notify(dispatchNotificationKey(scope.organization))
	s.stopMonthlyBudgetTurns(ctx, scope.organization)
	s.stopMonthlyBudgetSpritePasses(ctx, scope.organization)
	switch input.(type) {
	case monthlyBudgetRequest, spriteUsageRequest:
		s.resumeMonthlyBudgetWork(ctx, scope)
	case tracker.NativeRunEvent:
		if s.conversations != nil {
			if err := s.conversations.wakePending(ctx); err != nil {
				s.config.Logger.Warn("Resume cost-corrected conversations", "error", err)
			}
		}
	}
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Type == "run.finished" && scope.project != "" {
		s.startSpritePoolForQueue(scope)
	}
}

func (s *Service) waitNativeCandidates(c echo.Context) error {
	wait := 30
	if value := c.QueryParam("wait"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 30 {
			return s.nativeAPIError(c, nativeInvalid("Wait must be between 1 and 30 seconds"))
		}
		wait = parsed
	}
	scope := nativeRequestScope(c)
	key := dispatchNotificationKey(scope.organization)
	s.notifications.dispatchCursor(key)
	authorize := func() error {
		ctx := c.Request().Context()
		tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		now, err := s.database.currentTime()
		if err != nil {
			return err
		}
		if err := s.requireHostedProject(ctx, tx, scope, false); err != nil {
			return err
		}
		if err := requireRunnerAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		if err := authorizeNativeProject(ctx, tx, scope); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := authorize(); err != nil {
		return s.nativeAPIError(c, err)
	}
	subscription, cancel := s.notifications.subscribe(key)
	defer cancel()
	select {
	case <-subscription.closed:
		return c.NoContent(http.StatusServiceUnavailable)
	default:
	}
	cursor := s.notifications.dispatchCursor(key)
	if c.QueryParam("after") == cursor {
		timer := time.NewTimer(time.Duration(wait) * time.Second)
		defer timer.Stop()
		select {
		case <-c.Request().Context().Done():
			return c.Request().Context().Err()
		case <-subscription.closed:
			return c.NoContent(http.StatusServiceUnavailable)
		case <-timer.C:
		case <-subscription.wake:
		}
		if err := authorize(); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, struct {
		Cursor string `json:"cursor"`
	}{s.notifications.dispatchCursor(key)})
}
