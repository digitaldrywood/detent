package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestDoctorSystemdNetwork(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, scope, output string
		err                 error
		want                doctorStatus
		absent              bool
	}{
		{name: "ordered user unit", scope: "user", output: "LoadState=loaded\nAfter=basic.target network-online.target\nWants=network-online.target", want: doctorOK},
		{name: "missing both", scope: "system", output: "LoadState=loaded\nAfter=basic.target", want: doctorWarn},
		{name: "missing wants", output: "After=network-online.target", want: doctorWarn},
		{name: "missing after", output: "Wants=network-online.target", want: doctorWarn},
		{name: "similar target", output: "After=network-online.target.other\nWants=network-online.target", want: doctorWarn},
		{name: "not installed", output: "LoadState=not-found", absent: true},
		{name: "inspection failure", err: errors.New("systemctl unavailable"), want: doctorWarn},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runner := &serviceRunnerStub{status: servicepkg.Status{ServiceManager: servicepkg.ManagerSystemd, ServiceScope: tt.scope, Service: "detent.service"}}
			checks := checkDoctorSystemdNetwork(t.Context(), "/tmp/detent/global.yaml", options{service: serviceFactoryFor(runner), runCommand: func(_ context.Context, name string, args ...string) (string, error) {
				want := []string{"show", "detent.service", "--property=LoadState", "--property=After", "--property=Wants"}
				if tt.scope == "user" {
					want = append([]string{"--user"}, want...)
				}
				if name != "systemctl" || !reflect.DeepEqual(args, want) {
					t.Fatalf("command = %s %v", name, args)
				}
				return tt.output, tt.err
			}})
			if tt.absent {
				if len(checks) != 0 {
					t.Fatalf("checks = %v", checks)
				}
				return
			}
			if len(checks) != 1 || checks[0].Status != tt.want {
				t.Fatalf("checks = %v", checks)
			}
			if tt.want == doctorWarn && !strings.Contains(checks[0].Hint, "network-online.target") {
				t.Fatalf("missing repair hint: %v", checks)
			}
		})
	}
}
