package activehours

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateWindowMembership(t *testing.T) {
	t.Parallel()
	location := mustLocation(t, "America/Chicago")
	tests := []struct {
		name      string
		windows   []string
		now       time.Time
		wantOpen  bool
		wantClose time.Time
		wantNext  time.Time
	}{
		{
			name:      "inside same-day window",
			windows:   []string{"Mon-Fri 09:00-17:00"},
			now:       time.Date(2026, time.August, 7, 12, 0, 0, 0, location),
			wantOpen:  true,
			wantClose: time.Date(2026, time.August, 7, 17, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 10, 9, 0, 0, 0, location),
		},
		{
			name:      "outside same-day window",
			windows:   []string{"Mon-Fri 09:00-17:00"},
			now:       time.Date(2026, time.August, 7, 18, 0, 0, 0, location),
			wantClose: time.Date(2026, time.August, 10, 17, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 10, 9, 0, 0, 0, location),
		},
		{
			name:      "wrapping window after midnight",
			windows:   []string{"Mon-Fri 22:00-06:00"},
			now:       time.Date(2026, time.August, 8, 2, 0, 0, 0, location),
			wantOpen:  true,
			wantClose: time.Date(2026, time.August, 8, 6, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 10, 22, 0, 0, 0, location),
		},
		{
			name:      "weekday range excludes next night",
			windows:   []string{"Mon-Fri 22:00-06:00"},
			now:       time.Date(2026, time.August, 8, 22, 0, 0, 0, location),
			wantClose: time.Date(2026, time.August, 11, 6, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 10, 22, 0, 0, 0, location),
		},
		{
			name:      "inclusive start boundary",
			windows:   []string{"Fri-Fri 22:00-06:00"},
			now:       time.Date(2026, time.August, 7, 22, 0, 0, 0, location),
			wantOpen:  true,
			wantClose: time.Date(2026, time.August, 8, 6, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 14, 22, 0, 0, 0, location),
		},
		{
			name:      "exclusive close boundary",
			windows:   []string{"Fri-Fri 22:00-06:00"},
			now:       time.Date(2026, time.August, 8, 6, 0, 0, 0, location),
			wantClose: time.Date(2026, time.August, 15, 6, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 14, 22, 0, 0, 0, location),
		},
		{
			name:      "full weekend days merge",
			windows:   []string{"Sat-Sun 00:00-24:00"},
			now:       time.Date(2026, time.August, 9, 12, 0, 0, 0, location),
			wantOpen:  true,
			wantClose: time.Date(2026, time.August, 10, 0, 0, 0, 0, location),
			wantNext:  time.Date(2026, time.August, 15, 0, 0, 0, 0, location),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status, err := Evaluate(Config{Timezone: location.String(), Windows: test.windows}, test.now, time.Time{})
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if status.WindowOpen != test.wantOpen {
				t.Fatalf("WindowOpen = %t, want %t", status.WindowOpen, test.wantOpen)
			}
			if !status.NextClose.Equal(test.wantClose) {
				t.Errorf("NextClose = %v, want %v", status.NextClose, test.wantClose)
			}
			if !status.NextOpen.Equal(test.wantNext) {
				t.Errorf("NextOpen = %v, want %v", status.NextOpen, test.wantNext)
			}
		})
	}
}

func TestEvaluateWrappingWindowAcrossDST(t *testing.T) {
	t.Parallel()
	location := mustLocation(t, "America/Chicago")
	tests := []struct {
		name         string
		now          time.Time
		wantDuration time.Duration
	}{
		{
			name:         "spring forward shortens window",
			now:          time.Date(2026, time.March, 8, 1, 30, 0, 0, location),
			wantDuration: 7 * time.Hour,
		},
		{
			name:         "fall back lengthens window",
			now:          time.Date(2026, time.November, 1, 1, 30, 0, 0, location),
			wantDuration: 9 * time.Hour,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status, err := Evaluate(Config{Timezone: location.String(), Windows: []string{"Sat-Sat 22:00-06:00"}}, test.now, time.Time{})
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if !status.WindowOpen {
				t.Fatal("WindowOpen = false, want true")
			}
			if duration := status.NextClose.Sub(status.WindowStart); duration != test.wantDuration {
				t.Fatalf("window duration = %v, want %v", duration, test.wantDuration)
			}
		})
	}
}

func TestEvaluateOverride(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	config := Config{Timezone: "UTC", Windows: []string{"Mon-Fri 22:00-23:00"}}

	status, err := Evaluate(config, now, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if status.WindowOpen || !status.OverrideActive || !status.Open {
		t.Fatalf("status = %+v, want active override outside window", status)
	}

	status, err = Evaluate(config, now.Add(3*time.Hour), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Evaluate() after expiry error = %v", err)
	}
	if status.OverrideActive || status.Open {
		t.Fatalf("status = %+v, want expired override", status)
	}
}

func TestEvaluateAlwaysOpenScheduleHasNoArtificialBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	status, err := Evaluate(Config{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, now, time.Time{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !status.WindowOpen || !status.Open || !status.NextOpen.IsZero() || !status.NextClose.IsZero() {
		t.Fatalf("status = %+v, want always open without a transition", status)
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		config      Config
		wantProblem string
	}{
		{name: "zero config", config: Config{}},
		{name: "missing timezone", config: Config{Windows: []string{"Mon-Sun 22:00-06:00"}}, wantProblem: "timezone: is required"},
		{name: "invalid timezone", config: Config{Timezone: "Central", Windows: []string{"Mon-Sun 22:00-06:00"}}, wantProblem: "valid IANA timezone"},
		{name: "missing windows", config: Config{Timezone: "UTC"}, wantProblem: "must contain at least one"},
		{name: "invalid weekday", config: Config{Timezone: "UTC", Windows: []string{"Monday-Sun 22:00-06:00"}}, wantProblem: "weekday \"Monday\""},
		{name: "invalid clock", config: Config{Timezone: "UTC", Windows: []string{"Mon-Sun 9:00-06:00"}}, wantProblem: "zero-padded"},
		{name: "invalid end of day", config: Config{Timezone: "UTC", Windows: []string{"Mon-Sun 22:00-24:01"}}, wantProblem: "valid 24-hour time"},
		{name: "equal edges", config: Config{Timezone: "UTC", Windows: []string{"Mon-Sun 06:00-06:00"}}, wantProblem: "start and end must differ"},
		{name: "valid", config: Config{Timezone: "America/Chicago", Windows: []string{"Mon-Fri 22:00-06:00", "Sat-Sun 00:00-24:00"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			problems := test.config.Validate("active_hours")
			joined := strings.Join(problems, "; ")
			if test.wantProblem == "" && len(problems) != 0 {
				t.Fatalf("Validate() = %q, want no problems", joined)
			}
			if test.wantProblem != "" && !strings.Contains(joined, test.wantProblem) {
				t.Fatalf("Validate() = %q, want substring %q", joined, test.wantProblem)
			}
		})
	}
}

func TestParsePersistedOverride(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, time.August, 8, 2, 0, 0, 0, time.UTC)
	if got := ParsePersistedOverride(" 2026-08-08T02:00:00Z "); !got.Equal(want) {
		t.Fatalf("ParsePersistedOverride() = %v, want %v", got, want)
	}
	if got := ParsePersistedOverride("invalid"); !got.IsZero() {
		t.Fatalf("ParsePersistedOverride(invalid) = %v, want zero", got)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return location
}
