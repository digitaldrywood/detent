package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"strconv"
	"strings"
)

type launchdManager struct {
	cfg   Config
	label string
	path  string
}

func newLaunchdManager(cfg Config) *launchdManager {
	label := LaunchdLabel(cfg.Name)
	path := cfg.LaunchdPlistPath
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(cfg.HomeDir, "Library", "LaunchAgents", label+".plist")
	}
	return &launchdManager{cfg: cfg, label: label, path: path}
}

func (m *launchdManager) Info() ManagerInfo {
	return ManagerInfo{
		Name:           ManagerLaunchd,
		Scope:          "user",
		Unit:           m.label,
		DefinitionPath: m.path,
	}
}

func (m *launchdManager) Definition() Definition {
	stdout, stderr := serviceLogPaths(m.cfg)
	return Definition{Path: m.path, Content: launchdPlist(m.cfg), StandardOutPath: stdout, StandardErrorPath: stderr}
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
			output, queryErr := m.cfg.RunCommand(ctx, "launchctl", "print-disabled", m.domain())
			if queryErr == nil && launchdDisabled(output, m.label) {
				return fmt.Errorf("launchd label %s is disabled; enable it with `launchctl enable %s` and retry: %w", m.label, m.target(), bootstrapErr)
			}
			return bootstrapErr
		}
		return nil
	}
	_, err = m.cfg.RunCommand(ctx, "launchctl", "kickstart", m.target())
	return err
}

func launchdDisabled(output, label string) bool {
	// Match the quoted label exactly so a disabled board does not implicate
	// its runner. launchctl reports entries as "label" => disabled.
	for line := range strings.SplitSeq(output, "\n") {
		key, value, found := strings.Cut(line, "=>")
		if found && strings.TrimSpace(key) == strconv.Quote(label) {
			state := strings.TrimSpace(value)
			return state == "disabled"
		}
	}
	return false
}

func (m *launchdManager) Restart(ctx context.Context) error {
	if _, err := m.cfg.RunCommand(ctx, "launchctl", "kill", "SIGTERM", m.target()); err != nil {
		return err
	}
	if err := m.WaitStopped(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *launchdManager) WaitStopped(ctx context.Context) error {
	return waitStopped(ctx, m.cfg.PollInterval, m.Inspect)
}

func (m *launchdManager) domain() string {
	return "gui/" + m.cfg.UID
}

func (m *launchdManager) target() string {
	return m.domain() + "/" + m.label
}

func launchdPlist(cfg Config) string {
	stdout, stderr := serviceLogPaths(cfg)
	escape := func(value string) string {
		return html.EscapeString(value)
	}
	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key>`,
		`  <string>` + LaunchdLabel(cfg.Name) + `</string>`,
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
		`  <key>StandardOutPath</key>`,
		`  <string>`+escape(stdout)+`</string>`,
		`  <key>StandardErrorPath</key>`,
		`  <string>`+escape(stderr)+`</string>`,
		`  <key>ThrottleInterval</key>`,
		`  <integer>60</integer>`,
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
