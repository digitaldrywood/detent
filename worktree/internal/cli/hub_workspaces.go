package cli

import (
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

// hostedWorkspaceFileConfig is the optional `workspaces:` section of the hosted
// configuration file (decisions section 18.1). Every duration is a string so a
// file can say "30m" rather than a bare number whose unit nobody can see.
type hostedWorkspaceFileConfig struct {
	// Enabled mounts the workspace endpoints and the relay. With it off the
	// right panel's surfaces stay disabled with their reason (section 16).
	Enabled bool `yaml:"enabled"`
	// RequestTimeout is how long a workspace waits for an eligible runner
	// before it fails with no_runner. Default 5m.
	RequestTimeout string `yaml:"request_timeout"`
	// RetainAfterRun is how long an attempt's worktree stays available after
	// the run; past it a workspace on the attempt checks out head_sha fresh.
	// Default 30m.
	RetainAfterRun string `yaml:"retain_after_run"`
	// IdleTimeout is how long a workspace may sit without person-originated
	// activity. Default 30m.
	IdleTimeout string `yaml:"idle_timeout"`
	// MaxLifetime is the hard cap no workspace outlives. Default 4h.
	MaxLifetime string `yaml:"max_lifetime"`
	// Plan carries plan.workspaces.max_open.
	Plan *struct {
		MaxOpen int `yaml:"max_open"`
	} `yaml:"plan"`
	// PersonMaxOpen is the per-person cap. Section 18.1 fixes it at 3; the
	// key exists so an operator can lower it, never so one can be surprised
	// by a higher default.
	PersonMaxOpen int `yaml:"person_max_open"`
	// Relay carries workspaces.relay.memory, the hub process's relay budget.
	Relay *struct {
		Memory string `yaml:"memory"`
	} `yaml:"relay"`
	// Files carries workspaces.files.deny.
	Files *struct {
		Deny []string `yaml:"deny"`
	} `yaml:"files"`
	// Terminal carries workspaces.terminal.* (section 18.3).
	Terminal *struct {
		Enabled   bool   `yaml:"enabled"`
		Isolation string `yaml:"isolation"`
		// Record defaults to on, so the file has to say false to turn it
		// off; a pointer is the only way to tell "absent" from "false".
		Record *bool `yaml:"record"`
	} `yaml:"terminal"`
}

// readHostedWorkspaceConfig reads the `workspaces:` section. enabled is false
// when the section is absent or disabled.
func readHostedWorkspaceConfig(path string) (config hubserver.WorkspaceConfig, enabled bool, resultErr error) {
	if strings.TrimSpace(path) == "" {
		return hubserver.WorkspaceConfig{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return hubserver.WorkspaceConfig{}, false, errors.New("hosted configuration could not be opened")
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("hosted configuration could not be closed"))
		}
	}()
	decoder := yaml.NewDecoder(io.LimitReader(file, 128*1024))
	decoder.KnownFields(true)
	var section hostedFileConfig
	if err := decoder.Decode(&section); err != nil {
		return hubserver.WorkspaceConfig{}, false, errors.New("hosted configuration is invalid")
	}
	return readWorkspaceConfig(section.Workspaces)
}

// readWorkspaceConfig converts the file section into the hub's
// WorkspaceConfig. Absent durations are left at zero for the hub's own
// normalization to fill, so the defaults live in one place.
func readWorkspaceConfig(section *hostedWorkspaceFileConfig) (hubserver.WorkspaceConfig, bool, error) {
	if section == nil || !section.Enabled {
		return hubserver.WorkspaceConfig{}, false, nil
	}
	config := hubserver.WorkspaceConfig{Enabled: true, PersonMaxOpen: section.PersonMaxOpen}
	for _, field := range []struct {
		value  string
		target *time.Duration
		name   string
	}{
		{section.RequestTimeout, &config.RequestTimeout, "request_timeout"},
		{section.RetainAfterRun, &config.RetainAfterRun, "retain_after_run"},
		{section.IdleTimeout, &config.IdleTimeout, "idle_timeout"},
		{section.MaxLifetime, &config.MaxLifetime, "max_lifetime"},
	} {
		value := strings.TrimSpace(field.value)
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		// retain_after_run is the one that may legitimately be zero: an
		// operator who wants every workspace to check out fresh says "0s"
		// rather than having to pick a number small enough not to matter.
		if err != nil || duration < 0 || duration == 0 && field.name != "retain_after_run" {
			return hubserver.WorkspaceConfig{}, false, errors.New("workspaces " + field.name + " must be a positive duration")
		}
		*field.target = duration
	}
	if section.Plan != nil {
		if section.Plan.MaxOpen < 0 {
			return hubserver.WorkspaceConfig{}, false, errors.New("workspaces plan max_open must not be negative")
		}
		config.PlanMaxOpen = section.Plan.MaxOpen
	}
	if section.Relay != nil {
		memory, err := parseByteSize(section.Relay.Memory)
		if err != nil {
			return hubserver.WorkspaceConfig{}, false, errors.New("workspaces relay memory must be a byte size such as 256MB")
		}
		config.RelayMemoryBytes = memory
	}
	if section.Files != nil {
		config.FilesDeny = section.Files.Deny
	}
	if section.Terminal != nil {
		config.Terminal.Enabled = section.Terminal.Enabled
		config.Terminal.Isolation = strings.TrimSpace(section.Terminal.Isolation)
		// The pointer is carried through rather than dereferenced: the hub's
		// own normalization defaults a nil to on (section 18.3), so a file
		// that says nothing and a hub built in Go that says nothing reach the
		// same answer through the same line of code.
		config.Terminal.Record = section.Terminal.Record
	}
	normalized := config
	return normalized, true, nil
}

// parseByteSize reads a byte size written as a bare number of bytes or with a
// KB, MB or GB suffix. An empty value is zero, which the hub reads as "use the
// default".
func parseByteSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(strings.ToUpper(value))
	if trimmed == "" {
		return 0, nil
	}
	multiplier := int64(1)
	for _, suffix := range []struct {
		name  string
		scale int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if rest, ok := strings.CutSuffix(trimmed, suffix.name); ok {
			trimmed = strings.TrimSpace(rest)
			multiplier = suffix.scale
			break
		}
	}
	if trimmed == "" {
		return 0, errors.New("byte size has no number")
	}
	var total int64
	for _, digit := range trimmed {
		if digit < '0' || digit > '9' {
			return 0, errors.New("byte size is not a number")
		}
		total = total*10 + int64(digit-'0')
		if total > 1<<40 {
			return 0, errors.New("byte size is out of range")
		}
	}
	return total * multiplier, nil
}
