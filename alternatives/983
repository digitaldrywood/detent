package cloudentry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestScaleSoakAttachmentSweep(t *testing.T) {
	path := os.Getenv("DETENT_SCALE_SOAK_CONFIG")
	if path == "" {
		t.Skip("manual scale soak; see docs/scale-soak.md")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		AttachmentObjects int    `json:"attachment_objects"`
		SweepPasses       int    `json:"sweep_passes"`
		Pin               string `json:"pin"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.AttachmentObjects < 2 || config.SweepPasses < 1 || config.SweepPasses > 1000 {
		t.Fatal("attachment_objects must be >= 2 and sweep_passes in [1,1000]")
	}
	head, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(head)) != config.Pin {
		t.Fatal("pin must equal the checkout HEAD")
	}
	status, err := exec.CommandContext(t.Context(), "git", "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	diff, err := exec.CommandContext(t.Context(), "git", "diff", "HEAD", "--").Output()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(diff)
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	probe := &lockProbeStorage{Storage: storage, locks: &f.service.attachmentLocks, organization: "org_alpha"}
	f.service.attachments = probe
	organization, err := f.service.readyOrganization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-attachment.OrphanTTL - time.Hour)
	durations := make([]float64, 0, config.SweepPasses)
	failures := []string{}
	for range config.SweepPasses {
		store.mu.Lock()
		for index := range config.AttachmentObjects {
			key, err := attachment.Key(organization.ID, fmt.Sprintf("att_%032x", index+1))
			if err != nil {
				store.mu.Unlock()
				t.Fatal(err)
			}
			store.objects[key] = spacesObject{content: []byte("Example abandoned attachment"), created: old}
		}
		store.mu.Unlock()
		start := time.Now()
		err := f.service.sweepOrganizationAttachments(t.Context(), organization)
		durations = append(durations, float64(time.Since(start))/float64(time.Millisecond))
		if err != nil {
			failures = append(failures, err.Error())
		}
		store.mu.Lock()
		remaining := len(store.objects)
		store.mu.Unlock()
		if remaining != 0 {
			failures = append(failures, fmt.Sprintf("%d orphan objects remain", remaining))
		}
	}
	slices.Sort(durations)
	percentile := func(p int) float64 { return durations[(len(durations)*p+99)/100-1] }
	report := struct {
		Dirty             bool     `json:"dirty"`
		SourceDiffSHA256  string   `json:"source_diff_sha256"`
		GoVersion         string   `json:"go_version"`
		Commit            string   `json:"commit"`
		Objects           int      `json:"objects_per_pass"`
		Passes            int      `json:"passes"`
		P50MS             float64  `json:"p50_ms"`
		P95MS             float64  `json:"p95_ms"`
		P99MS             float64  `json:"p99_ms"`
		MaxMS             float64  `json:"max_ms"`
		ObjectVisits      int      `json:"object_visits"`
		ReaderLockBlocked int      `json:"reader_lock_blocked_visits"`
		Failures          []string `json:"errors"`
		Limitations       string   `json:"limitations"`
	}{len(status) > 0, hex.EncodeToString(digest[:]), runtime.Version(), config.Pin, config.AttachmentObjects, config.SweepPasses, percentile(50), percentile(95), percentile(99), durations[len(durations)-1], probe.visits, probe.blocked, failures, "isolated Cloud Entry and real tenant Hubs; in-memory S3 transport; separate from runner load; excludes network latency"}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "attachment-sweep-report.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("attachment_sweep_report=%s", encoded)
	if len(failures) > 0 {
		t.Fatal("attachment sweep failed")
	}
}
