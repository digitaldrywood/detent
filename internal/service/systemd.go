package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const systemdUnitName = "detent.service"

type systemdManager struct {
	cfg      Config
	userPath string
	scope    string
	path     string
}

func newSystemdManager(cfg Config) *systemdManager {
	userPath := cfg.UserSystemdPath
	if strings.TrimSpace(userPath) == "" {
		configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
		if configHome == "" {
			configHome = filepath.Join(cfg.HomeDir, ".config")
		}
		userPath = filepath.Join(configHome, "systemd", "user", systemdUnitName)
	}
	return &systemdManager{cfg: cfg, userPath: userPath, scope: "user", path: userPath}
}

func (m *systemdManager) Info() ManagerInfo {
	return ManagerInfo{
		Name:           ManagerSystemd,
		Scope:          m.scope,
		Unit:           systemdUnitName,
		DefinitionPath: m.path,
	}
}

func (m *systemdManager) Definition() Definition {
	return Definition{Path: m.userPath, Content: systemdUnit(m.cfg)}
}

func (m *systemdManager) Inspect(ctx context.Context) (Inspection, error) {
	scope, path, present := m.definitionLocation()
	m.scope = scope
	m.path = path
	if !present {
		for _, candidate := range []string{"user", "system"} {
			m.scope = candidate
			inspection, loaded, err := m.inspectLoaded(ctx)
			if err == nil && loaded {
				m.path = ""
				return inspection, nil
			}
		}
		m.scope = "user"
		m.path = m.userPath
		return Inspection{State: StateStopped}, nil
	}
	inspection, loaded, err := m.inspectLoaded(ctx)
	if err != nil {
		return Inspection{}, err
	}
	if !loaded {
		inspection.Present = true
	}
	return inspection, nil
}

func (m *systemdManager) inspectLoaded(ctx context.Context) (Inspection, bool, error) {
	args := m.systemctlArgs("show", systemdUnitName, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID")
	output, err := m.cfg.RunCommand(ctx, "systemctl", args...)
	if err != nil {
		return Inspection{}, false, err
	}
	properties := parseProperties(output)
	if loadState := strings.ToLower(strings.TrimSpace(properties["LoadState"])); loadState == "" || loadState == "not-found" {
		return Inspection{State: StateStopped}, false, nil
	}
	pid, err := strconv.Atoi(properties["MainPID"])
	if err != nil {
		pid = 0
	}
	return Inspection{
		Present:   true,
		State:     systemdState(properties["ActiveState"], properties["SubState"]),
		PID:       pid,
		StartedAt: processStartedAt(ctx, m.cfg.RunCommand, pid),
	}, true, nil
}

func (m *systemdManager) Install(ctx context.Context) (Definition, error) {
	definition := m.Definition()
	if err := writeDefinition(definition); err != nil {
		return Definition{}, err
	}
	m.scope = "user"
	m.path = definition.Path
	if _, err := m.cfg.RunCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return Definition{}, err
	}
	if _, err := m.cfg.RunCommand(ctx, "systemctl", "--user", "enable", systemdUnitName); err != nil {
		return Definition{}, err
	}
	return definition, nil
}

func (m *systemdManager) Start(ctx context.Context) error {
	_, err := m.cfg.RunCommand(ctx, "systemctl", m.systemctlArgs("start", systemdUnitName)...)
	return err
}

func (m *systemdManager) Restart(ctx context.Context) error {
	_, err := m.cfg.RunCommand(ctx, "systemctl", m.systemctlArgs("restart", systemdUnitName)...)
	return err
}

func (m *systemdManager) WaitStopped(ctx context.Context) error {
	return waitStopped(ctx, m.cfg.PollInterval, m.Inspect)
}

func (m *systemdManager) definitionLocation() (string, string, bool) {
	userPaths := append([]string{m.userPath}, m.cfg.UserUnitPaths...)
	if strings.TrimSpace(m.cfg.HomeDir) != "" {
		userPaths = append(userPaths, filepath.Join(m.cfg.HomeDir, ".local", "share", "systemd", "user", systemdUnitName))
	}
	userPaths = append(userPaths,
		filepath.Join("/etc", "systemd", "user", systemdUnitName),
		filepath.Join("/usr", "lib", "systemd", "user", systemdUnitName),
	)
	for _, path := range userPaths {
		if regularFile(path) {
			return "user", path, true
		}
	}
	paths := m.cfg.SystemUnitPaths
	if len(paths) == 0 {
		paths = []string{
			filepath.Join("/etc", "systemd", "system", systemdUnitName),
			filepath.Join("/usr", "lib", "systemd", "system", systemdUnitName),
			filepath.Join("/lib", "systemd", "system", systemdUnitName),
		}
	}
	for _, path := range paths {
		if regularFile(path) {
			return "system", path, true
		}
	}
	return "user", m.userPath, false
}

func (m *systemdManager) systemctlArgs(args ...string) []string {
	if m.scope != "system" {
		return append([]string{"--user"}, args...)
	}
	return args
}

func systemdUnit(cfg Config) string {
	execStart := systemdQuote(cfg.BinaryPath)
	for _, argument := range cfg.Arguments {
		execStart += " " + systemdQuote(argument)
	}
	return strings.Join([]string{
		"[Unit]",
		"Description=Detent agent orchestrator",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"Environment=\"PATH=" + systemdEscape(cfg.Path) + "\"",
		"ExecStart=" + execStart,
		"Restart=on-failure",
		"RestartSec=5",
		"KillMode=control-group",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

func systemdQuote(value string) string {
	return strconv.Quote(strings.ReplaceAll(value, "%", "%%"))
}

func systemdEscape(value string) string {
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	return strings.ReplaceAll(value, "\"", "\\\"")
}

func parseProperties(output string) map[string]string {
	properties := make(map[string]string)
	for line := range strings.SplitSeq(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return properties
}

func systemdState(activeState string, subState string) State {
	switch strings.ToLower(strings.TrimSpace(activeState)) {
	case "active":
		return StateRunning
	case "activating":
		return StateStarting
	case "deactivating":
		return StateStopping
	case "inactive", "failed":
		return StateStopped
	}
	switch strings.ToLower(strings.TrimSpace(subState)) {
	case "running", "listening":
		return StateRunning
	case "start", "start-pre", "start-post":
		return StateStarting
	case "stop", "stop-sigterm", "stop-sigkill", "final-sigterm", "final-sigkill":
		return StateStopping
	case "dead", "failed", "exited":
		return StateStopped
	default:
		return StateUnknown
	}
}

func waitStopped(ctx context.Context, interval time.Duration, inspect func(context.Context) (Inspection, error)) error {
	for {
		status, err := inspect(ctx)
		if err != nil {
			return err
		}
		if !active(status.State) && status.State != StateStopping {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("wait for service stop: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
