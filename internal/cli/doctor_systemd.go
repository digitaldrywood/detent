package cli

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

// Inspect effective dependencies so system and user units, including drop-ins,
// are checked using the same service discovery as detent status.
func checkDoctorSystemdNetwork(ctx context.Context, configPath string, opts options) []doctorCheck {
	factory := opts.service
	if factory == nil {
		factory = defaultServiceFactory
	}
	run := opts.runCommand
	if run == nil {
		run = defaultCommandRunner
	}
	runner, err := factory(servicepkg.Config{GOOS: runtime.GOOS, ConfigPath: configPath, LockPath: filepath.Join(filepath.Dir(configPath), "detent.db.lock"), RunCommand: servicepkg.CommandRunner(run)})
	if err != nil {
		return nil
	}
	status, err := runner.Status(ctx)
	if err != nil || status.ServiceManager != servicepkg.ManagerSystemd {
		return nil
	}
	args := []string{"show", status.Service, "--property=LoadState", "--property=After", "--property=Wants"}
	if status.ServiceScope == "user" {
		args = append([]string{"--user"}, args...)
	}
	output, err := run(ctx, "systemctl", args...)
	check := doctorCheck{Name: "Systemd network ordering", Status: doctorWarn, Hint: "Add After=network-online.target and Wants=network-online.target to the installed unit's [Unit] section, then run systemctl daemon-reload (with --user for a user service)."}
	if err != nil {
		check.Detail = "Cannot inspect systemd network dependencies: " + err.Error()
		return []doctorCheck{check}
	}
	properties := map[string]string{}
	for line := range strings.SplitSeq(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if properties["LoadState"] == "not-found" {
		return nil
	}
	var missing []string
	for _, key := range []string{"After", "Wants"} {
		found := false
		for _, target := range strings.Fields(properties[key]) {
			if target == "network-online.target" {
				found = true
			}
		}
		if !found {
			missing = append(missing, key+"=network-online.target")
		}
	}
	if len(missing) == 0 {
		check.Status = doctorOK
		check.Detail = "Installed systemd unit orders startup after and requests network-online.target"
		check.Hint = ""
	} else {
		check.Detail = "Installed systemd unit lacks " + strings.Join(missing, " and ")
	}
	return []doctorCheck{check}
}
