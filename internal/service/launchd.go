package service

import (
	"context"
	"errors"
	"html"
	"path/filepath"
	"strconv"
	"strings"
)

const launchdLabel = "com.digitaldrywood.detent"

type launchdManager struct {
	cfg  Config
	path string
}

func newLaunchdManager(cfg Config) *launchdManager {
	path := cfg.LaunchdPlistPath
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(cfg.HomeDir, "Library", "LaunchAgents", launchdLabel+".plist")
	}
	return &launchdManager{cfg: cfg, path: path}
}

func (m *launchdManager) Info() ManagerInfo {
	return ManagerInfo{
		Name:           ManagerLaunchd,
		Scope:          "user",
		Unit:           launchdLabel,
		DefinitionPath: m.path,
	}
}

func (m *launchdManager) Definition() Definition {
	return Definition{Path: m.path, Content: launchdPlist(m.cfg)}
}

func (m *launchdManager) Inspect(ctx context.Context) (Inspection, error) {
	present := regularFile(m.path)
	output, err := m.cfg.RunCommand(ctx, "launchctl", "print", m.target())
	if err != nil {
		if launchdMissing(output, err) {
			return Inspection{Present: present, State: StateStopped}, nil
		}
		if !present {
			return Inspection{State: StateStopped}, nil
		}
		return Inspection{}, err
	}
	pid := launchdPID(output)
	return Inspection{
		Present:   true,
		State:     launchdState(output),
		PID:       pid,
		StartedAt: processStartedAt(ctx, m.cfg.RunCommand, pid),
	}, nil
}

func (m *launchdManager) Install(context.Context) (Definition, error) {
	definition := m.Definition()
	if err := writeDefinition(definition); err != nil {
		return Definition{}, err
	}
	return definition, nil
}

func (m *launchdManager) Start(ctx context.Context) error {
	inspection, err := m.Inspect(ctx)
	if err != nil {
		return err
	}
	if active(inspection.State) {
		return nil
	}
	if _, err := m.cfg.RunCommand(ctx, "launchctl", "print", m.target()); err != nil {
		if _, bootstrapErr := m.cfg.RunCommand(ctx, "launchctl", "bootstrap", m.domain(), m.path); bootstrapErr != nil {
			return bootstrapErr
		}
		return nil
	}
	_, err = m.cfg.RunCommand(ctx, "launchctl", "kickstart", m.target())
	return err
}

func (m *launchdManager) Restart(ctx context.Context) error {
	_, err := m.cfg.RunCommand(ctx, "launchctl", "kickstart", "-k", m.target())
	return err
}

func (m *launchdManager) WaitStopped(ctx context.Context) error {
	return waitStopped(ctx, m.cfg.PollInterval, m.Inspect)
}

func (m *launchdManager) domain() string {
	return "gui/" + m.cfg.UID
}

func (m *launchdManager) target() string {
	return m.domain() + "/" + launchdLabel
}

func launchdPlist(cfg Config) string {
	escape := func(value string) string {
		return html.EscapeString(value)
	}
	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key>`,
		`  <string>` + launchdLabel + `</string>`,
		`  <key>ProgramArguments</key>`,
		`  <array>`,
		`    <string>` + escape(cfg.BinaryPath) + `</string>`,
	}
	for _, argument := range cfg.Arguments {
		lines = append(lines, `    <string>`+escape(argument)+`</string>`)
	}
	lines = append(lines, `  </array>`)
	if workingDirectory := strings.TrimSpace(cfg.HomeDir); workingDirectory != "" {
		lines = append(lines,
			`  <key>WorkingDirectory</key>`,
			`  <string>`+escape(workingDirectory)+`</string>`,
		)
	}
	lines = append(lines,
		`  <key>EnvironmentVariables</key>`,
		`  <dict>`,
		`    <key>PATH</key>`,
		`    <string>`+escape(cfg.Path)+`</string>`,
		`    <key>`+ManagerEnvironment+`</key>`,
		`    <string>`+string(ManagerLaunchd)+`</string>`,
		`  </dict>`,
		`  <key>RunAtLoad</key>`,
		`  <true/>`,
		`  <key>KeepAlive</key>`,
		`  <dict>`,
		`    <key>SuccessfulExit</key>`,
		`    <false/>`,
		`  </dict>`,
		`</dict>`,
		`</plist>`,
		``,
	)
	return strings.Join(lines, "\n")
}

func launchdMissing(output string, err error) bool {
	text := strings.ToLower(strings.TrimSpace(output + " " + err.Error()))
	return strings.Contains(text, "could not find service") ||
		strings.Contains(text, "service not found") ||
		strings.Contains(text, "no such process")
}

func launchdPID(output string) int {
	for line := range strings.SplitSeq(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "pid" {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0
		}
		return pid
	}
	return 0
}

func launchdState(output string) State {
	for line := range strings.SplitSeq(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "state" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "running":
			return StateRunning
		case "waiting", "spawn scheduled":
			return StateStarting
		case "terminating":
			return StateStopping
		case "exited", "not running":
			return StateStopped
		}
	}
	if launchdPID(output) > 0 {
		return StateRunning
	}
	return StateStopped
}

func validateLaunchdConfig(cfg Config) error {
	if strings.TrimSpace(cfg.UID) == "" {
		return errors.New("resolve launchd user domain: user id is unavailable")
	}
	return nil
}
