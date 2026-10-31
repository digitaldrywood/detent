package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

type dailySummaryPreference struct {
	Enabled    bool       `json:"enabled"`
	Configured bool       `json:"configured"`
	SendTime   string     `json:"send_time"`
	TimeZone   string     `json:"time_zone"`
	Days       string     `json:"days"`
	NextSend   *time.Time `json:"next_send"`
	Recipient  string     `json:"recipient"`
}

func (p dailySummaryPreference) validate() (int, *time.Location, error) {
	at, err := time.Parse("15:04", p.SendTime)
	if err != nil || at.Minute()%30 != 0 {
		return 0, nil, nativePublicInvalid("Choose a send time on the hour or half hour")
	}
	zone, err := time.LoadLocation(p.TimeZone)
	if err != nil || p.TimeZone == "" || p.TimeZone == "Local" {
		return 0, nil, nativePublicInvalid("Choose an IANA time zone")
	}
	if p.Days != "every_day" && p.Days != "weekdays" {
		return 0, nil, nativePublicInvalid("Choose Every day or Weekdays")
	}
	return at.Hour()*60 + at.Minute(), zone, nil
}

func summarySchedule(at time.Time, minute int, zone *time.Location, weekdays bool, direction int) time.Time {
	local := at.In(zone)
	for offset := range 9 {
		day := time.Date(local.Year(), local.Month(), local.Day()+offset*direction, 12, 0, 0, 0, zone)
		if weekdays && (day.Weekday() == time.Saturday || day.Weekday() == time.Sunday) {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(), minute/60, minute%60, 0, 0, zone)
		wall := candidate.In(zone)
		if wall.Hour()*60+wall.Minute() < minute {
			candidate = candidate.Add(time.Duration(minute-wall.Hour()*60-wall.Minute()) * time.Minute)
		}
		if direction > 0 && candidate.After(at) || direction < 0 && candidate.Before(at) {
			return candidate.UTC()
		}
	}
	panic("daily summary schedule has no weekday")
}

func summaryJitter(user, organization string, end time.Time) time.Duration {
	sum := sha256.Sum256([]byte(user + "\x00" + organization + "\x00" + end.UTC().Format(time.RFC3339)))
	return time.Duration(binary.BigEndian.Uint32(sum[:4])%181) * time.Second
}

func (s *Service) dailySummaryPreference(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" {
		return s.hostedJSONError(c, http.StatusForbidden, "Use your own account to change notification preferences")
	}
	user, org := credential.Hosted.Subject, s.config.Hosted.OrganizationID
	value := dailySummaryPreference{SendTime: "06:00", TimeZone: "UTC", Days: "every_day"}
	if c.Request().Method == http.MethodPut {
		var request struct {
			Enabled  *bool  `json:"enabled"`
			SendTime string `json:"send_time"`
			TimeZone string `json:"time_zone"`
			Days     string `json:"days"`
		}
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		if request.Enabled == nil {
			return s.nativeAPIError(c, nativePublicInvalid("An enabled preference is required"))
		}
		value = dailySummaryPreference{Enabled: *request.Enabled, SendTime: request.SendTime, TimeZone: request.TimeZone, Days: request.Days}
		if err := s.database.saveDailySummaryPreference(ctx, user, org, value, s.config.now()); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var minute int
	var weekdays bool
	var end int64
	err = s.database.reader.QueryRowContext(readCtx, `SELECT p.enabled,p.send_minute,p.time_zone,p.weekdays,p.window_end,m.email FROM hosted_members m LEFT JOIN daily_summary_preferences p ON p.user_id=m.user_id AND p.organization_id=? WHERE m.user_id=? AND p.user_id IS NOT NULL`, org, user).Scan(&value.Enabled, &minute, &value.TimeZone, &weekdays, &end, &value.Recipient)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.database.reader.QueryRowContext(readCtx, "SELECT email FROM hosted_members WHERE user_id=?", user).Scan(&value.Recipient)
	} else if err == nil {
		value.Configured = true
		value.SendTime = fmt.Sprintf("%02d:%02d", minute/60, minute%60)
		value.Days = "every_day"
		if weekdays {
			value.Days = "weekdays"
		}
		if value.Enabled {
			next := time.Unix(end, 0).UTC()
			value.NextSend = &next
		}
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) unsubscribeDailySummary(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	token := c.QueryParam("token")
	if c.Request().Method == http.MethodPost {
		var request struct {
			Token string `json:"token"`
		}
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		token = request.Token
	}
	if !validNativeID(token, "unsub") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	result, err := s.database.db.ExecContext(c.Request().Context(), `UPDATE daily_summary_preferences SET enabled=0 WHERE token=? AND organization_id=?`, token, s.config.Hosted.OrganizationID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if count == 0 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if c.Request().Method == http.MethodPost {
		return c.NoContent(http.StatusNoContent)
	}
	return c.String(http.StatusOK, "You are unsubscribed from this organization's daily summary.")
}

func (d *database) saveDailySummaryPreference(ctx context.Context, user, org string, value dailySummaryPreference, now time.Time) error {
	minute, zone, err := value.validate()
	if err != nil {
		return err
	}
	next := summarySchedule(now, minute, zone, value.Days == "weekdays", 1)
	_, err = d.db.ExecContext(ctx, `INSERT INTO daily_summary_preferences(user_id,organization_id,enabled,send_minute,time_zone,weekdays,token,window_end,due_at) VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,organization_id) DO UPDATE SET enabled=excluded.enabled,send_minute=excluded.send_minute,time_zone=excluded.time_zone,weekdays=excluded.weekdays,window_end=excluded.window_end,due_at=excluded.due_at`, user, org, value.Enabled, minute, value.TimeZone, value.Days == "weekdays", newNativeID("unsub"), next.Unix(), next.Add(summaryJitter(user, org, next)).Unix())
	if err != nil {
		return fmt.Errorf("save daily summary preference: %w", err)
	}
	return nil
}
