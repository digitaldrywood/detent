package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestCheckDoctorProjectActiveHours(t *testing.T) {
	t.Parallel()
	location, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	config := activehours.Config{Timezone: location.String(), Windows: []string{"Mon-Fri 22:00-06:00"}}
	tests := []struct {
		name        string
		now         time.Time
		project     globalconfig.Project
		wantDetails []string
	}{
		{
			name: "outside window previews opening and closing in both zones",
			now:  time.Date(2026, time.August, 7, 12, 0, 0, 0, location),
			wantDetails: []string{
				"off hours in America/Chicago",
				"next open 2026-08-07 22:00 CDT (2026-08-08 03:00 UTC)",
				"next close 2026-08-08 06:00 CDT (2026-08-08 11:00 UTC)",
			},
		},
		{
			name: "manual pause remains stronger inside window",
			now:  time.Date(2026, time.August, 7, 23, 0, 0, 0, location),
			project: globalconfig.Project{
				Paused: true,
			},
			wantDetails: []string{
				"window open in America/Chicago",
				"current close 2026-08-08 06:00 CDT (2026-08-08 11:00 UTC)",
				"following open 2026-08-10 22:00 CDT (2026-08-11 03:00 UTC)",
				"manual pause still blocks dispatch",
			},
		},
		{
			name: "override is previewed",
			now:  time.Date(2026, time.August, 7, 12, 0, 0, 0, location),
			project: globalconfig.Project{
				ActiveHoursOverrideUntil: "2026-08-07T19:00:00Z",
			},
			wantDetails: []string{"override active until 2026-08-07 14:00 CDT (2026-08-07 19:00 UTC)", "off hours"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workflow := workflowconfig.Config{ActiveHours: config}
			check, ok := checkDoctorProjectActiveHours("detent", test.project, workflow, doctorDeps{now: func() time.Time { return test.now }})
			if !ok {
				t.Fatal("checkDoctorProjectActiveHours() ok = false")
			}
			if check.Status != doctorOK {
				t.Fatalf("Status = %q, want OK", check.Status)
			}
			for _, want := range test.wantDetails {
				if !strings.Contains(check.Detail, want) {
					t.Errorf("Detail = %q, want containing %q", check.Detail, want)
				}
			}
		})
	}
}
