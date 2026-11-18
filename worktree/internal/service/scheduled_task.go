package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/digitaldrywood/detent/internal/shell"
)

type scheduledTaskManager struct {
	cfg Config
}

func newScheduledTaskManager(cfg Config) *scheduledTaskManager {
	return &scheduledTaskManager{cfg: cfg}
}

func (m *scheduledTaskManager) Info() ManagerInfo {
	return ManagerInfo{
		Name:           ManagerScheduledTask,
		Scope:          "user",
		Unit:           serviceName(m.cfg.Name) + "-" + m.cfg.UID,
		DefinitionPath: filepath.Join(filepath.Dir(m.cfg.ConfigPath), serviceName(m.cfg.Name)+".task.xml"),
	}
}

func (m *scheduledTaskManager) Definition() Definition {
	stdout, stderr := LogPaths(m.cfg)
	return Definition{Path: m.Info().DefinitionPath, Content: scheduledTaskXML(m.cfg), StandardOutPath: stdout, StandardErrorPath: stderr}
}

func (m *scheduledTaskManager) Inspect(ctx context.Context) (Inspection, error) {
	output, err := m.run(ctx, `try { $task = $folder.GetTask(`+shell.QuotePowerShellArg(m.Info().Unit)+`) } catch { if ($_.Exception.GetBaseException().HResult -eq -2147024894) { 'missing'; exit 0 }; throw }; [int]$task.State`)
	if err != nil {
		return Inspection{}, err
	}
	if strings.TrimSpace(output) == "missing" {
		return Inspection{State: StateStopped}, nil
	}
	state, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return Inspection{}, fmt.Errorf("read Scheduled Task state: %w", err)
	}
	inspection := Inspection{Present: true, State: StateUnknown}
	switch state {
	case 1, 3:
		inspection.State = StateStopped
	case 2:
		inspection.State = StateStarting
	case 4:
		inspection.State = StateRunning
	}
	return inspection, nil
}

func (m *scheduledTaskManager) Install(ctx context.Context) (Definition, error) {
	if strings.TrimSpace(m.cfg.UID) == "" {
		return Definition{}, errors.New("resolve Scheduled Task principal: user id is unavailable")
	}
	definition := m.Definition()
	if err := writeDefinition(definition); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return Definition{}, err
		}
		inspection, inspectErr := m.Inspect(ctx)
		if inspectErr != nil {
			return Definition{}, inspectErr
		}
		info, statErr := os.Lstat(definition.Path)
		if inspection.Present || statErr != nil || !info.Mode().IsRegular() {
			return Definition{}, errors.Join(err, statErr)
		}
		if err := os.Remove(definition.Path); err != nil {
			return Definition{}, err
		}
		if err := writeDefinition(definition); err != nil {
			return Definition{}, err
		}
	}
	_, err := m.run(ctx, `$xml = [System.IO.File]::ReadAllText(`+shell.QuotePowerShellArg(definition.Path)+`); $null = $folder.RegisterTask(`+shell.QuotePowerShellArg(m.Info().Unit)+`, $xml, 2, `+shell.QuotePowerShellArg(m.cfg.UID)+`, $null, 3, $null)`)
	if err != nil {
		return Definition{}, errors.Join(err, os.Remove(definition.Path))
	}
	return definition, nil
}

func (m *scheduledTaskManager) Start(ctx context.Context) error {
	_, err := m.run(ctx, `$task = $folder.GetTask(`+shell.QuotePowerShellArg(m.Info().Unit)+`); if ($task.State -ne 4) { $null = $task.Run($null) }`)
	return err
}

func (m *scheduledTaskManager) Stop(ctx context.Context) error {
	_, err := m.run(ctx, `$folder.GetTask(`+shell.QuotePowerShellArg(m.Info().Unit)+`).Stop(0)`)
	if err != nil {
		return err
	}
	return m.WaitStopped(ctx)
}

func (m *scheduledTaskManager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	return m.Start(ctx)
}

func (m *scheduledTaskManager) Uninstall(ctx context.Context) error {
	inspection, err := m.Inspect(ctx)
	if err != nil {
		return err
	}
	if inspection.Present {
		if err := m.Stop(ctx); err != nil {
			return err
		}
		if _, err := m.run(ctx, `$folder.DeleteTask(`+shell.QuotePowerShellArg(m.Info().Unit)+`, 0)`); err != nil {
			return err
		}
	}
	if err := os.Remove(m.Info().DefinitionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *scheduledTaskManager) WaitStopped(ctx context.Context) error {
	return waitStopped(ctx, m.cfg.PollInterval, m.Inspect)
}

func (m *scheduledTaskManager) run(ctx context.Context, script string) (string, error) {
	return m.cfg.RunCommand(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(`$ErrorActionPreference = 'Stop'; $scheduler = New-Object -ComObject 'Schedule.Service'; $scheduler.Connect(); $folder = $scheduler.GetFolder('\'); `+script))
}

func scheduledTaskXML(cfg Config) string {
	arguments := make([]string, 0, len(cfg.Arguments))
	for _, argument := range cfg.Arguments {
		arguments = append(arguments, shell.QuotePowerShellArg(argument))
	}
	script := `$env:` + ManagerEnvironment + ` = ` + shell.QuotePowerShellArg(string(ManagerScheduledTask)) + `; $env:DETENT_SERVICE_TASK = ` + shell.QuotePowerShellArg(serviceName(cfg.Name)+"-"+cfg.UID) + `; $env:PATH = ` + shell.QuotePowerShellArg(cfg.Path) + `; & ` + shell.QuotePowerShellArg(cfg.BinaryPath) + ` ` + strings.Join(arguments, " ") + `; exit $LASTEXITCODE`
	return `<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + html.EscapeString(cfg.UID) + `</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Runner"><UserId>` + html.EscapeString(cfg.UID) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Runner"><Exec><Command>powershell.exe</Command><Arguments>-NoProfile -NonInteractive -EncodedCommand ` + encodePowerShell(script) + `</Arguments><WorkingDirectory>` + html.EscapeString(cfg.HomeDir) + `</WorkingDirectory></Exec></Actions>
</Task>
`
}

func encodePowerShell(script string) string {
	words := utf16.Encode([]rune(script))
	encoded := make([]byte, len(words)*2)
	for i, word := range words {
		binary.LittleEndian.PutUint16(encoded[i*2:], word)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}
