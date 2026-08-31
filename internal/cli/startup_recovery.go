package cli

import (
	"context"
	"os"
	"runtime"
	"strings"

	"github.com/digitaldrywood/detent/internal/notify"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func newDefaultStartupRecovery(_ context.Context, cfg BootConfig) (StartupRecovery, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	version := runtimeUpdateVersion(cfg)
	statePath := detentupdate.RecoveryStatePath(cfg.Global.Path)
	preflight := candidateStartupPreflight(cfg)
	applyOptions := detentupdate.ApplyOptions{
		AssumeYes:         true,
		Preflight:         preflight,
		RecoveryStatePath: statePath,
	}

	var updater detentupdate.Updater
	autoUpdate := cfg.Global.Update.AutoCheckEnabled && cfg.Global.Update.AutoApplyEnabled
	if autoUpdate {
		updater = newRuntimeUpdater(cfg, executable, version)
	}
	hostname, hostnameErr := os.Hostname()
	if hostnameErr != nil {
		hostname = ""
	}
	instance := strings.TrimSpace(cfg.Global.InstanceName)
	if instance == "" {
		instance = strings.TrimSpace(cfg.Global.Global.Identity.Name)
	}
	if instance == "" {
		instance = strings.TrimSpace(strings.SplitN(hostname, ".", 2)[0])
	}

	return detentupdate.NewStartupRecovery(detentupdate.StartupRecoveryConfig{
		StatePath:      statePath,
		CurrentVersion: version,
		ExecutablePath: executable,
		GOOS:           runtime.GOOS,
		AutoUpdate:     autoUpdate,
		Updater:        updater,
		ApplyOptions:   applyOptions,
		Instance:       instance,
		Host:           hostname,
		Notify:         startupCrashLoopNotifier(cfg),
	})
}

func newRuntimeUpdater(cfg BootConfig, executable string, version string) detentupdate.Updater {
	return detentupdate.NewService(detentupdate.Config{
		CurrentVersion: version,
		ExecutablePath: executable,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		Client: detentupdate.NewGitHubClient(detentupdate.GitHubClientConfig{
			Token: strings.TrimSpace(cfg.Runtime.GitHubToken.Value),
		}),
	})
}

func candidateStartupPreflight(cfg BootConfig) detentupdate.BinaryPreflight {
	args := []string{
		"--config", cfg.Global.Path,
		"--port", "0",
		"--format", "json",
		"doctor", "--startup-preflight",
	}
	return detentupdate.CommandPreflight(args...)
}

func startupCrashLoopNotifier(cfg BootConfig) func(context.Context, detentupdate.CrashLoopEvent) error {
	health := cfg.Global.Notifications.Health
	if strings.TrimSpace(health.Webhook.URL) == "" {
		return nil
	}
	webhook, err := notify.NewWebhook(notify.WebhookConfig{
		URL:     health.Webhook.URL,
		Headers: health.Webhook.Headers,
		Timeout: health.Webhook.Timeout(),
	})
	if err != nil {
		return func(context.Context, detentupdate.CrashLoopEvent) error { return err }
	}
	return func(ctx context.Context, event detentupdate.CrashLoopEvent) error {
		return webhook.Send(ctx, event)
	}
}

func runtimeUpdateVersion(cfg BootConfig) string {
	version := strings.TrimSpace(cfg.Version)
	if version == "" {
		version = strings.TrimSpace(cfg.Build.Version)
	}
	return version
}
