package hubserver

import (
	"testing"
	"time"
)

func TestDailySummarySchedule(t *testing.T) {
	zone, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, after, want, previous string
		weekdays                    bool
		hours                       float64
	}{
		{"spring change", "2026-03-07T06:00:00-06:00", "2026-03-08T06:00:00-05:00", "2026-03-07T06:00:00-06:00", false, 23},
		{"fall change", "2026-10-31T06:00:00-05:00", "2026-11-01T06:00:00-06:00", "2026-10-31T06:00:00-05:00", false, 25},
		{"weekend", "2026-10-23T06:00:00-05:00", "2026-10-26T06:00:00-05:00", "2026-10-23T06:00:00-05:00", true, 72},
		{"weekday DST", "2026-10-30T06:00:00-05:00", "2026-11-02T06:00:00-06:00", "2026-10-30T06:00:00-05:00", true, 73},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parse := func(s string) time.Time {
				at, err := time.Parse(time.RFC3339, s)
				if err != nil {
					t.Fatal(err)
				}
				return at
			}
			next := summarySchedule(parse(tt.after), 360, zone, tt.weekdays, 1)
			prev := summarySchedule(next, 360, zone, tt.weekdays, -1)
			if !next.Equal(parse(tt.want)) || !prev.Equal(parse(tt.previous)) || next.Sub(prev).Hours() != tt.hours {
				t.Fatalf("window %s..%s", prev, next)
			}
			if again := summarySchedule(next, 360, zone, tt.weekdays, 1); !again.After(next) {
				t.Fatal("same window scheduled twice")
			}

		})
	}
	for _, tt := range []struct {
		name, send, zone, days string
		valid                  bool
	}{{"half hour", "06:30", "America/Chicago", "weekdays", true}, {"hour", "23:00", "UTC", "every_day", true}, {"minute", "06:15", "UTC", "every_day", false}, {"invalid time", "24:00", "UTC", "every_day", false}, {"zone", "06:00", "Bad/Zone", "every_day", false}, {"local zone", "06:00", "Local", "every_day", false}, {"days", "06:00", "UTC", "sunday", false}} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := (dailySummaryPreference{SendTime: tt.send, TimeZone: tt.zone, Days: tt.days}).validate()
			if (err == nil) != tt.valid {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}
