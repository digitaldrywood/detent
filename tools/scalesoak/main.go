package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

type config struct {
	Projects          int    `json:"projects"`
	Items             int    `json:"items"`
	Attempts          int    `json:"attempts"`
	Runners           int    `json:"runners"`
	DetailBytes       int    `json:"detail_bytes"`
	Duration          string `json:"duration"`
	Pin               string `json:"pin"`
	SeedOnly          bool   `json:"seed_only"`
	AttachmentObjects int    `json:"attachment_objects"`
	SweepPasses       int    `json:"sweep_passes"`
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("scalesoak", flag.ContinueOnError)
	flags.SetOutput(output)
	var c config
	flags.IntVar(&c.Projects, "projects", 2, "projects in the tenant database")
	flags.IntVar(&c.Items, "items", 100, "work items in the tenant database")
	flags.IntVar(&c.Attempts, "attempts", 1000, "historical attempts with archived details")
	flags.IntVar(&c.Runners, "runners", 10, "concurrent simulated runners")
	flags.IntVar(&c.DetailBytes, "detail-bytes", 8192, "execution output bytes per historical attempt")
	duration := flags.Duration("duration", 10*time.Minute, "fleet observation duration after seeding")
	flags.StringVar(&c.Pin, "pin", "", "required checkout commit SHA")
	flags.BoolVar(&c.SeedOnly, "seed-only", false, "create the fixture without fleet traffic or sweep")
	flags.IntVar(&c.AttachmentObjects, "attachment-objects", 16000, "abandoned objects per entry-plane sweep pass")
	flags.IntVar(&c.SweepPasses, "sweep-passes", 3, "entry-plane sweep samples")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || c.Pin == "" || *duration <= 0 || c.AttachmentObjects < 2 || c.SweepPasses < 1 || c.SweepPasses > 1000 {
		return errors.New("require -pin SHA, positive duration, attachment-objects >= 2, sweep-passes in [1,1000], and no positional arguments")
	}
	scratch := ""
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if scratch = os.Getenv(key); scratch != "" {
			break
		}
	}
	if scratch == "" {
		return errors.New("set TMPDIR, TMP, or TEMP to an isolated scratch directory")
	}
	root, err := os.MkdirTemp(scratch, "detent-scale-soak-control-")
	if err != nil {
		return err
	}
	c.Duration = duration.String()
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "scale_soak_output=%s\n", root); err != nil {
		return err
	}
	jobs := []struct {
		name    string
		command *exec.Cmd
	}{
		{"hub", exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "-timeout=0", "./internal/hubserver", "-run", "^TestScaleSoak$")},
	}
	if !c.SeedOnly {
		jobs = append(jobs, struct {
			name    string
			command *exec.Cmd
		}{"entry", exec.CommandContext(ctx, "go", "test", "-count=1", "-v", "-timeout=0", "./internal/cloudentry", "-run", "^TestScaleSoakAttachmentSweep$")})
	}
	for _, job := range jobs {
		command := job.command
		command.Env = append(os.Environ(), "TMPDIR="+root, "DETENT_SCALE_SOAK_CONFIG="+path)
		command.Stdout, command.Stderr = output, output
		command.WaitDelay = 10 * time.Second
		procgroup.Configure(ctx, command)
		if err := procgroup.Start(command); err != nil {
			return fmt.Errorf("start %s: %w", job.name, err)
		}
		group := procgroup.GroupID(command)
		if err := procgroup.Wait(command); err != nil {
			return fmt.Errorf("run %s: %w", job.name, errors.Join(err, procgroup.Cleanup(group)))
		}
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
