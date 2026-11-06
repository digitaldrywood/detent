package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
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
	repository, ok := s.config.AccessRepository.(accountstore.Repository)
	if !ok {
		return s.nativeAPIError(c, errors.New("account repository unavailable"))
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
		minute, zone, saveErr := value.validate()
		if saveErr == nil {
			next := summarySchedule(s.config.now(), minute, zone, value.Days == "weekdays", 1)
			saveErr = repository.SetDailySummaryPreference(ctx, s.accessOrganization(), user, credential.HostedMembership, credential.ID, accountstore.DailySummaryPreference{
				Enabled: value.Enabled, SendMinute: minute, TimeZone: value.TimeZone, Weekdays: value.Days == "weekdays", WindowEnd: next.Unix(),
			}, newNativeID("unsub"), next.Add(summaryJitter(user, org, next)).Unix())
		}
		if saveErr != nil {
			return s.nativeAPIError(c, saveErr)
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	preference, err := repository.DailySummaryPreference(readCtx, s.accessOrganization(), user, credential.HostedMembership, credential.ID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value.Recipient = preference.Recipient
	if preference.Configured {
		value.Configured = true
		value.Enabled = preference.Enabled
		value.SendTime = fmt.Sprintf("%02d:%02d", preference.SendMinute/60, preference.SendMinute%60)
		value.TimeZone = preference.TimeZone
		value.Days = "every_day"
		if preference.Weekdays {
			value.Days = "weekdays"
		}
		if value.Enabled {
			next := time.Unix(preference.WindowEnd, 0).UTC()
			value.NextSend = &next
		}
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
	repository, ok := s.config.AccessRepository.(accountstore.Repository)
	if !ok {
		return s.nativeAPIError(c, errors.New("account repository unavailable"))
	}
	if err := repository.UnsubscribeDailySummary(c.Request().Context(), s.accessOrganization(), token); err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Request().Method == http.MethodPost {
		return c.NoContent(http.StatusNoContent)
	}
	return c.String(http.StatusOK, "You are unsubscribed from this organization's daily summary.")
}
