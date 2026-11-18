package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

func TestRunnerServiceProviderHeartbeat(t *testing.T) {
	for _, test := range []struct {
		name      string
		fresh     bool
		providers bool
		other     bool
		delayed   bool
		readError bool
		wantError string
	}{
		{name: "provider heartbeat", fresh: true, providers: true},
		{name: "registration report is not service readiness", providers: true, wantError: "no new runner heartbeat"},
		{name: "heartbeat without provider", fresh: true, wantError: "reported no providers"},
		{name: "another runner", fresh: true, providers: true, other: true, wantError: "no new runner heartbeat"},
		{name: "wait for service heartbeat", fresh: true, providers: true, delayed: true},
		{name: "Hub read fails", readError: true, wantError: "read runner before service start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				baseline := time.Now()
				reads := 0
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				stub := &serviceRunnerStub{startResults: []servicepkg.StartResult{{Action: servicepkg.ActionStarted}}}
				service := runnerHeartbeatService{ServiceRunner: stub, cfg: globalconfig.Config{Path: "/runner/global.yaml"}, id: "runner", read: func(context.Context) (runnerauth.Fleet, error) {
					reads++
					if test.readError {
						return runnerauth.Fleet{}, errors.New("unavailable")
					}
					runner := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: "runner"}, LastHeartbeatAt: baseline}
					if reads > 1 && test.fresh && (!test.delayed || reads > 2) {
						runner.LastHeartbeatAt = baseline.Add(time.Second)
					}
					if test.providers {
						runner.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{Provider: "openai"}}}
					}
					if test.other {
						runner.RunnerID = "another"
					}
					return runnerauth.Fleet{Runners: []runnerauth.Runner{runner}}, nil
				}}
				_, err := service.Start(ctx, servicepkg.StartOptions{Install: true})
				if !test.readError && len(stub.startOptions) != 1 {
					t.Fatalf("service starts = %d", len(stub.startOptions))
				}
				if test.wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("heartbeat check = %v, want %s", err, test.wantError)
				}
			})
		})
	}
}
