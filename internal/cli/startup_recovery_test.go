package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func TestRootCommandHandlesBootFailureWithStartupRecovery(t *testing.T) {
	t.Parallel()

	bootErr := errors.New("startup validation failed")
	configPath := t.TempDir() + "/global.yaml"
	global := validDoctorGlobalWithProjects(configPath)
	recovery := &recordingStartupRecovery{}
	cmd := NewRootCommand(context.Background(),
		func(opts *options) {
			configured := successfulDoctorOptionsWithConfig(configPath, global)
			opts.resolvePath = configured.resolvePath
			opts.read = configured.read
			opts.startupRecovery = func(_ context.Context, cfg BootConfig) (StartupRecovery, error) {
				if cfg.Mode != BootModeRunning || cfg.Global.Path != configPath {
					t.Fatalf("startup recovery config = %#v, want running %s", cfg, configPath)
				}
				return recovery, nil
			}
		},
		WithVersion("0.93.0"),
		WithBootFunc(func(_ context.Context, cfg BootConfig) error {
			if cfg.StartupRecovery != recovery {
				t.Fatal("BootConfig.StartupRecovery was not injected")
			}
			return bootErr
		}),
	)
	cmd.SetArgs([]string{"--config", configPath, "--headless", "--port", "0"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := cmd.Execute()
	if !errors.Is(err, bootErr) {
		t.Fatalf("Execute() error = %v, want %v", err, bootErr)
	}
	if len(recovery.failures) != 1 || !errors.Is(recovery.failures[0], bootErr) {
		t.Fatalf("HandleFailure() errors = %v, want startup error", recovery.failures)
	}
}

func TestStartupCrashLoopNotifierUsesHealthWebhook(t *testing.T) {
	t.Parallel()

	var body bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		_, _ = io.Copy(&body, request.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	cfg := BootConfig{Global: globalconfig.Config{
		Notifications: globalconfig.Notifications{
			Health: globalconfig.HealthNotifications{
				Webhook: globalconfig.NotificationWebhook{
					URL:       server.URL,
					TimeoutMS: int(time.Second / time.Millisecond),
				},
			},
		},
	}}
	notify := startupCrashLoopNotifier(cfg)
	if notify == nil {
		t.Fatal("startupCrashLoopNotifier() = nil, want configured webhook")
	}
	event := detentupdate.CrashLoopEvent{
		Event:        "detent_startup_crash_loop",
		Version:      "0.93.0",
		FailureCount: 3,
		Error:        "startup validation failed",
	}
	if err := notify(context.Background(), event); err != nil {
		t.Fatalf("notify() error = %v", err)
	}

	var got detentupdate.CrashLoopEvent
	if err := json.Unmarshal(body.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.Event != event.Event || got.Version != event.Version || got.FailureCount != event.FailureCount || got.Error != event.Error {
		t.Fatalf("webhook event = %#v, want %#v", got, event)
	}
}

type recordingStartupRecovery struct {
	healthyCalls int
	failures     []error
}

func (r *recordingStartupRecovery) MarkHealthy(context.Context) error {
	r.healthyCalls++
	return nil
}

func (r *recordingStartupRecovery) HandleFailure(_ context.Context, err error) {
	r.failures = append(r.failures, err)
}
