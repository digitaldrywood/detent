//go:build detent_scalesoak

package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestScaleSoak(t *testing.T) {
	runScaleSoak(t)
}

func runScaleSoak(t *testing.T) {
	path := os.Getenv("DETENT_SCALE_SOAK_CONFIG")
	if path == "" {
		t.Fatal("manual scale soak requires DETENT_SCALE_SOAK_CONFIG; use go run ./tools/scalesoak")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config scaleSoakConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	duration, err := time.ParseDuration(config.Duration)
	if err != nil || duration <= 0 {
		t.Fatal("duration must be positive")
	}
	head, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	pin := strings.TrimSpace(string(head))
	if config.Pin != pin {
		t.Fatal("pin must equal the checkout HEAD")
	}
	diff, err := exec.CommandContext(t.Context(), "git", "diff", "HEAD", "--").Output()
	if err != nil {
		t.Fatal(err)
	}
	status, err := exec.CommandContext(t.Context(), "git", "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(diff)
	root, err := os.MkdirTemp(filepath.Dir(path), "tenant-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture_root=%s", root)
	seedStart := time.Now()
	seed := seedScaleSoak(t, root, config)
	seedDuration := time.Since(seedStart)
	metrics := newScaleSoakMetrics()
	var fleetErrors []error
	if !config.SeedOnly {
		ctx, cancel := context.WithTimeout(t.Context(), duration)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				scaleSoakMaintenance(ctx, seed, metrics)
				if !scaleSoakWait(ctx, time.Minute) {
					return
				}
			}
		}()
		fleetErrors = runScaleSoakFleet(ctx, seed, 0, 30*time.Second, metrics)
		cancel()
		<-done
	}
	metrics.summaries()
	report := struct {
		Commit           string            `json:"commit"`
		SourceDiffSHA256 string            `json:"source_diff_sha256"`
		Dirty            bool              `json:"dirty"`
		GoVersion        string            `json:"go_version"`
		Platform         string            `json:"platform"`
		Config           scaleSoakConfig   `json:"config"`
		SeedSeconds      float64           `json:"seed_seconds"`
		Metrics          *scaleSoakMetrics `json:"metrics"`
		Limitations      []string          `json:"limitations"`
	}{pin, hex.EncodeToString(digest[:]), len(status) > 0, runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH, config, seedDuration.Seconds(), metrics,
		[]string{"isolated PostgreSQL database; one populated tenant; local disk object storage; no model execution", "explicit maintenance passes; entry-plane object retention sweep is a separate fixture", "percentiles are upper bounds from power-of-two microsecond histogram buckets"}}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("scale_soak_report=%s", encoded)
	if metrics.OperationErrors > 0 || slices.ContainsFunc(fleetErrors, func(err error) bool { return err != nil }) {
		t.Fatalf("soak errors: %v; %v", fleetErrors, metrics.Failures)
	}
}
