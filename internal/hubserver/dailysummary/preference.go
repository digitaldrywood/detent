package dailysummary

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

type Preference struct {
	Enabled    bool       `json:"enabled"`
	Configured bool       `json:"configured"`
	SendTime   string     `json:"send_time"`
	TimeZone   string     `json:"time_zone"`
	Days       string     `json:"days"`
	NextSend   *time.Time `json:"next_send"`
	Recipient  string     `json:"recipient"`
}

func (p Preference) validate() (int, *time.Location, error) {
	at, err := time.Parse("15:04", p.SendTime)
	if err != nil || at.Minute()%30 != 0 {
		return 0, nil, hubapi.PublicInvalid("Choose a send time on the hour or half hour")
	}
	zone, err := time.LoadLocation(p.TimeZone)
	if err != nil || p.TimeZone == "" || p.TimeZone == "Local" {
		return 0, nil, hubapi.PublicInvalid("Choose an IANA time zone")
	}
	if p.Days != "every_day" && p.Days != "weekdays" {
		return 0, nil, hubapi.PublicInvalid("Choose Every day or Weekdays")
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

type Member struct {
	User, Membership, Credential string
}

type PreferenceUpdate struct {
	Enabled  *bool  `json:"enabled"`
	SendTime string `json:"send_time"`
	TimeZone string `json:"time_zone"`
	Days     string `json:"days"`
}

func (s *Service) Accounts() error {
	if _, ok := s.deps.AccessRepository.(accountstore.Repository); !ok {
		return errors.New("account repository unavailable")
	}
	return nil
}

func (s *Service) Preference(ctx context.Context, member Member, update *PreferenceUpdate) (Preference, error) {
	repository, ok := s.deps.AccessRepository.(accountstore.Repository)
	if !ok {
		return Preference{}, errors.New("account repository unavailable")
	}
	user, org := member.User, string(s.deps.Organization())
	value := Preference{SendTime: "06:00", TimeZone: "UTC", Days: "every_day"}
	if update != nil {
		if update.Enabled == nil {
			return Preference{}, hubapi.PublicInvalid("An enabled preference is required")
		}
		value = Preference{Enabled: *update.Enabled, SendTime: update.SendTime, TimeZone: update.TimeZone, Days: update.Days}
		minute, zone, saveErr := value.validate()
		if saveErr == nil {
			next := summarySchedule(s.deps.Now(), minute, zone, value.Days == "weekdays", 1)
			saveErr = repository.SetDailySummaryPreference(ctx, s.deps.Organization(), user, member.Membership, member.Credential, accountstore.DailySummaryPreference{
				Enabled: value.Enabled, SendMinute: minute, TimeZone: value.TimeZone, Weekdays: value.Days == "weekdays", WindowEnd: next.Unix(),
			}, hubapi.NewID("unsub"), next.Add(summaryJitter(user, org, next)).Unix())
		}
		if saveErr != nil {
			return Preference{}, saveErr
		}
	}
	preference, err := repository.DailySummaryPreference(ctx, s.deps.Organization(), user, member.Membership, member.Credential)
	if err != nil {
		return Preference{}, err
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
	return value, nil
}

func (s *Service) Unsubscribe(ctx context.Context, token string) error {
	repository, ok := s.deps.AccessRepository.(accountstore.Repository)
	if !ok {
		return errors.New("account repository unavailable")
	}
	return repository.UnsubscribeDailySummary(ctx, s.deps.Organization(), token)
}
