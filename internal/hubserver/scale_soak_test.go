package hubserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scaleSoakConfig struct {
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

func (c scaleSoakConfig) validate() error {
	if c.Projects < 1 || c.Items < c.Projects || c.Runners < 1 || c.Items < c.Runners || c.Attempts < 0 || c.DetailBytes < 1 || c.DetailBytes > 1<<20 {
		return errors.New("require projects >= 1, items >= projects and runners, runners >= 1, attempts >= 0, and detail_bytes in [1,1048576]")
	}
	return nil
}

type scaleSoakDetails struct{ root string }

func (s scaleSoakDetails) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if err := ctx.Err(); err != nil {
		return attachment.ArchiveReference{}, err
	}
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	path := filepath.Join(s.root, filepath.FromSlash(ref.Key))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return ref, err
	}
	return ref, os.WriteFile(path, compressed, 0600)
}

func (s scaleSoakDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	compressed, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(ref.Key)))
	if err != nil {
		return nil, err
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}

type scaleSoakLogHandler struct {
	slog.Handler
	boardErrors *atomic.Int64
}

func (h scaleSoakLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelError || h.Handler.Enabled(ctx, level)
}

func (h scaleSoakLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "refresh native board card" && record.Level >= slog.LevelError {
		h.boardErrors.Add(1)
	}
	return h.Handler.Handle(ctx, record)
}

func (h scaleSoakLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Handler = h.Handler.WithAttrs(attrs)
	return h
}

func (h scaleSoakLogHandler) WithGroup(name string) slog.Handler {
	h.Handler = h.Handler.WithGroup(name)
	return h
}

type scaleSoakSeed struct {
	service     *Service
	fixtures    []nativeFixture
	items       []tracker.NativeIssue
	runners     []runnerFixture
	details     scaleSoakDetails
	boardErrors *atomic.Int64
}

func seedScaleSoak(t *testing.T, root string, config scaleSoakConfig) scaleSoakSeed {
	t.Helper()
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	details := scaleSoakDetails{root: filepath.Join(root, "objects")}
	boardErrors := &atomic.Int64{}
	logger := slog.New(scaleSoakLogHandler{Handler: slog.DiscardHandler, boardErrors: boardErrors})
	service := openTestService(t, Config{Logger: logger, DatabasePath: filepath.Join(root, "hub.db"), RunDetails: details, Conversation: &ConversationConfig{Enabled: true}})
	seed := scaleSoakSeed{service: service, details: details, boardErrors: boardErrors}
	for i := range config.Projects {
		f := newNativeFixture(t, service, "", fmt.Sprintf("scale-project-%d", i))
		approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
		seed.fixtures = append(seed.fixtures, f)
	}
	for i := range config.Items {
		seed.items = append(seed.items, seed.fixtures[i%config.Projects].create(t, fmt.Sprintf("scale-item-%d", i)))
	}
	for i := range config.Runners {
		f := seed.fixtures[i%config.Projects]
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
		r.enroll(t)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/machines/register", r.redemption.Credential,
			map[string]any{"id": r.binding.MachineID, "hostname": "scale-host", "version": "test", "capacity": 1, "backend_isolation": r.redemption.BackendIsolation}), http.StatusOK)
		seed.runners = append(seed.runners, r)
	}
	seedScaleSoakAttempts(t, seed, config)
	return seed
}

func seedScaleSoakAttempts(t *testing.T, seed scaleSoakSeed, config scaleSoakConfig) {
	t.Helper()
	type prepared struct {
		item tracker.NativeIssue
		data tracker.NativeRunData
		ref  attachment.ArchiveReference
	}
	now := formatHubTime(time.Now().UTC().Add(-24 * time.Hour))
	var uploader string
	if err := seed.service.database.reader.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE scope='admin' LIMIT 1").Scan(&uploader); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < config.Attempts; offset += 100 {
		batch := make([]prepared, 0, 100)
		for index := offset; index < min(offset+100, config.Attempts); index++ {
			item := seed.items[index%len(seed.items)]
			data := tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), Sequence: 3, PolicyID: hubTestPolicy().ID, Outcome: "succeeded", CompletionBody: strings.Repeat("Example execution output. ", config.DetailBytes/25+1)[:config.DetailBytes], Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}}
			batch = append(batch, prepared{item: item, data: data})
		}
		tx, err := seed.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := range batch {
			row := &batch[i]
			lease := newNativeID("lease")
			result, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at)
SELECT ?,id,?,?,?,?,?,?,?,? FROM issues WHERE native_id=?`, lease, seed.runners[0].binding.MachineID, newNativeID("session"), now, now, now, now, now, now, row.item.WorkItemID)
			if err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
			fence, err := result.LastInsertId()
			if err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
			row.data.LeaseID, row.data.FencingToken = tracker.LeaseID(lease), tracker.FencingToken(fence)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for i := range batch {
			row := &batch[i]
			raw, err := json.Marshal(map[string]tracker.NativeRunData{runDetailEntry(row.data): row.data})
			if err != nil {
				t.Fatal(err)
			}
			row.ref, err = seed.details.StoreDetail(t.Context(), string(row.item.OrganizationID), raw)
			if err != nil {
				t.Fatal(err)
			}
		}
		tx, err = seed.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range batch {
			detail := newRunDetailReference(row.ref, string(row.item.OrganizationID), runDetailEntry(row.data))
			compact, err := json.Marshal(storedRun{NativeRunData: compactRun(row.data), Detail: detail})
			if err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
			_, err = tx.ExecContext(t.Context(), `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, detail.AttachmentID, row.item.OrganizationID, row.item.ProjectID, uploader, "archive.gz", "application/gzip", row.ref.CompressedSize, row.ref.SHA256, now, "archive", row.ref.Retention, row.ref.Key, row.ref.UncompressedSize, row.ref.Codec, row.ref.SchemaVersion)
			if err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
			_, err = tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at,work_item_revision,dispatch_generation,detail_attachment_id,policy_id,provider,model,execution_role,final_outcome)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.data.AttemptID, row.item.OrganizationID, row.item.ProjectID, row.item.WorkItemID, row.data.LeaseID, row.data.FencingToken, row.data.RunID, 3, "succeeded", string(compact), now, now, row.item.Revision, 1, detail.AttachmentID, row.data.PolicyID, "codex", "test-model", "implement", "succeeded")
			if err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
			_, err = tx.ExecContext(t.Context(), `INSERT INTO attachment_archive_references(organization_id,project_id,work_item_id,attachment_id) VALUES(?,?,?,?)`, row.item.OrganizationID, row.item.ProjectID, row.item.WorkItemID, detail.AttachmentID)
			if err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

type scaleSoakDistribution struct {
	Count     int64     `json:"count"`
	Errors    int64     `json:"errors"`
	ErrorRate float64   `json:"error_rate"`
	P50MS     float64   `json:"p50_ms"`
	P95MS     float64   `json:"p95_ms"`
	P99MS     float64   `json:"p99_ms"`
	MaxMS     float64   `json:"max_ms"`
	Buckets   [64]int64 `json:"-"`
}

func (d *scaleSoakDistribution) add(duration time.Duration, failed bool) {
	d.Count++
	if failed {
		d.Errors++
	}
	micros := (max(int64(1), int64(duration)) + 999) / 1000
	bucket := 0
	for micros > 1 && bucket < len(d.Buckets)-1 {
		micros = (micros + 1) / 2
		bucket++
	}
	d.Buckets[bucket]++
	d.MaxMS = max(d.MaxMS, float64(duration)/float64(time.Millisecond))
}

func (d scaleSoakDistribution) summary() scaleSoakDistribution {
	percentile := func(p int64) float64 {
		target := (d.Count*p + 99) / 100
		var count int64
		for i, n := range d.Buckets {
			count += n
			if count >= target && target > 0 {
				return float64(uint64(1)<<i) / 1000
			}
		}
		return 0
	}
	d.P50MS, d.P95MS, d.P99MS = percentile(50), percentile(95), percentile(99)
	if d.Count > 0 {
		d.ErrorRate = float64(d.Errors) / float64(d.Count)
	}
	return d
}

type scaleSoakMetrics struct {
	OperationErrors int64 `json:"non_cancellation_errors"`
	mu              sync.Mutex
	Requests        map[string]scaleSoakDistribution `json:"requests"`
	Writer          scaleSoakDistribution            `json:"writer_hold"`
	Jobs            map[string]scaleSoakDistribution `json:"explicit_maintenance_passes"`
	Failures        []string                         `json:"first_errors,omitempty"`
}

func newScaleSoakMetrics() *scaleSoakMetrics {
	return &scaleSoakMetrics{Requests: map[string]scaleSoakDistribution{}, Jobs: map[string]scaleSoakDistribution{}}
}

func (m *scaleSoakMetrics) record(name string, start time.Time, err error, job, canceled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	target := m.Requests
	if job {
		target = m.Jobs
	}
	d := target[name]
	d.add(time.Since(start), err != nil)
	target[name] = d
	if err != nil && !canceled {
		m.OperationErrors++
	}
	if err != nil && len(m.Failures) < 10 {
		m.Failures = append(m.Failures, name+": "+err.Error())
	}
}

func (m *scaleSoakMetrics) middleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)
		timing := writerTimingFrom(c.Request().Context())
		if timing != nil {
			m.mu.Lock()
			m.Writer.add(time.Duration(timing.hold.Load()), false)
			m.mu.Unlock()
		}
		return err
	}
}

func (m *scaleSoakMetrics) summaries() {
	m.Writer = m.Writer.summary()
	for name, d := range m.Requests {
		m.Requests[name] = d.summary()
	}
	for name, d := range m.Jobs {
		m.Jobs[name] = d.summary()
	}
}

type scaleSoakHTTP struct {
	client  *http.Client
	url     string
	metrics *scaleSoakMetrics
}

func (h scaleSoakHTTP) post(ctx context.Context, name, path, token string, body, result any) (err error) {
	start := time.Now()
	defer func() {
		h.metrics.record(name, start, err, false, scaleSoakCanceled(ctx, err))
		if err != nil {
			err = fmt.Errorf("%s: %w", name, err)
		}
	}()
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 2048))
		return errors.Join(fmt.Errorf("status %d: %s", response.StatusCode, data), readErr)
	}
	if result != nil {
		return json.NewDecoder(response.Body).Decode(result)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func scaleSoakCanceled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

func scaleSoakWait(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return ctx.Err() == nil
	}
	delay := interval * time.Duration(800+rand.IntN(401)) / 1000
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runScaleSoakRunner(ctx context.Context, h scaleSoakHTTP, r runnerFixture, item tracker.NativeIssue, cycles int, cadence time.Duration) error {
	if !scaleSoakWait(ctx, time.Duration(rand.IntN(1000))*cadence/1000) {
		return nil
	}
	heartbeat := func() error {
		report := capacityReport(time.Now().UTC())
		report.SharedAccountAlias = r.binding.RunnerID
		return h.post(ctx, "heartbeat", r.nativeFixture.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
			map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "scale-runner", "capacity": 1, "version": "test", "provider_reports": []any{report}}, nil)
	}
	if err := heartbeat(); err != nil {
		return err
	}
	var lease tracker.NativeLease
	if err := h.post(ctx, "claim", r.nativeFixture.base+"/claims", r.redemption.Credential, providerClaim(r, item, newNativeID("session")), &lease); err != nil {
		return err
	}
	event := nativeStartedEvent(lease)
	event.IdempotencyKey = newNativeID("event")
	events := r.nativeFixture.base + "/work-items/" + string(item.WorkItemID) + "/events"
	if err := h.post(ctx, "run.started", events, r.redemption.Credential, event, nil); err != nil {
		return err
	}
	for cycle := 0; cycles == 0 || cycle < cycles; cycle++ {
		if !scaleSoakWait(ctx, cadence) {
			return nil
		}
		if err := heartbeat(); err != nil {
			return err
		}
		if err := h.post(ctx, "renew", r.nativeFixture.base+"/leases/"+string(lease.ID)+"/renew", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}, &lease); err != nil {
			return err
		}
		event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", newNativeID("event"), event.Data.Sequence+1
		event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
		prior := event
		event.IdempotencyKey, event.Data.Sequence = newNativeID("event"), event.Data.Sequence+1
		event.PriorEvents = []tracker.NativeRunEvent{prior}
		if err := h.post(ctx, "run.observed", events, r.redemption.Credential, event, nil); err != nil {
			return err
		}
		event.PriorEvents = nil
	}
	return nil
}

func runScaleSoakFleet(ctx context.Context, seed scaleSoakSeed, cycles int, cadence time.Duration, metrics *scaleSoakMetrics) []error {
	seed.service.echo.Use(metrics.middleware)
	server := httptest.NewServer(seed.service.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 60 * time.Second
	defer client.CloseIdleConnections()
	h := scaleSoakHTTP{client: client, url: server.URL, metrics: metrics}
	results := make([]error, len(seed.runners))
	var workers sync.WaitGroup
	for i, r := range seed.runners {
		workers.Go(func() {
			err := runScaleSoakRunner(ctx, h, r, seed.items[i], cycles, cadence)
			if !scaleSoakCanceled(ctx, err) {
				results[i] = err
			}
		})
	}
	workers.Wait()
	return results
}

func scaleSoakMaintenance(ctx context.Context, seed scaleSoakSeed, metrics *scaleSoakMetrics) {
	jobs := []struct {
		name string
		run  func() error
	}{
		{"health_detector", func() error { return seed.service.evaluateTenantHealth(ctx) }},
		{"board_refresh", func() error {
			before := seed.boardErrors.Load()
			seed.service.maintainNativeBoard(ctx)
			if seed.boardErrors.Load() != before {
				return errors.New("board refresh failed; Hub logged an error")
			}
			return ctx.Err()
		}},
		{"conversation_attachment_sweep", func() error { _, err := seed.service.conversations.sweepAttachments(ctx); return err }},
	}
	for _, job := range jobs {
		start := time.Now()
		err := job.run()
		metrics.record(job.name, start, err, true, scaleSoakCanceled(ctx, err))
	}
}

func TestScaleSoakSeed(t *testing.T) {
	config := scaleSoakConfig{Projects: 2, Items: 4, Attempts: 6, Runners: 2, DetailBytes: 1024}
	seed := seedScaleSoak(t, t.TempDir(), config)
	for _, test := range []struct {
		table string
		want  int
	}{{"projects", 2}, {"issues", 4}, {"native_attempts", 6}, {"attachments", 6}, {"runner_identities", 2}} {
		var count int
		if err := seed.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM "+test.table).Scan(&count); err != nil || count != test.want {
			t.Fatalf("%s count=%d want=%d: %v", test.table, count, test.want, err)
		}
	}
	rows, err := seed.service.database.reader.QueryContext(t.Context(), "SELECT project_id,data_json FROM native_attempts ORDER BY fencing_token")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ctx := seed.service.runDetailContext(t.Context())
	seen := map[tracker.ProjectID]bool{}
	for rows.Next() {
		var raw string
		var project tracker.ProjectID
		if err := rows.Scan(&project, &raw); err != nil {
			t.Fatal(err)
		}
		var stored storedRun
		if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Detail == nil {
			t.Fatalf("missing detail: %v", err)
		}
		var data tracker.NativeRunData
		if err := decodeRunDetail(ctx, seed.service.database.reader, raw, &data); err != nil || len(data.CompletionBody) < config.DetailBytes {
			t.Fatalf("unreadable detail: %v", err)
		}
		seen[project] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("seeded projects=%d", len(seen))
	}
}

func TestScaleSoakFleet(t *testing.T) {
	seed := seedScaleSoak(t, t.TempDir(), scaleSoakConfig{Projects: 2, Items: 4, Attempts: 2, Runners: 2, DetailBytes: 512})
	metrics := newScaleSoakMetrics()
	for _, err := range runScaleSoakFleet(t.Context(), seed, 2, 0, metrics) {
		if err != nil {
			t.Fatal(err)
		}
	}
	scaleSoakMaintenance(t.Context(), seed, metrics)
	metrics.summaries()
	for name, want := range map[string]int64{"heartbeat": 6, "claim": 2, "renew": 4, "run.started": 2, "run.observed": 4} {
		got := metrics.Requests[name]
		if got.Count != want || got.Errors != 0 {
			t.Fatalf("%s=%+v want count=%d", name, got, want)
		}
	}
	if metrics.Writer.Count != 18 {
		t.Fatalf("writer observations=%d", metrics.Writer.Count)
	}
	for name, job := range metrics.Jobs {
		if job.Count != 1 || job.Errors != 0 {
			t.Fatalf("%s=%+v", name, job)
		}
	}
	var attempts int
	if err := seed.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE status='running' AND sequence=5").Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("live attempts=%d: %v", attempts, err)
	}
}

func TestScaleSoak(t *testing.T) {
	path := os.Getenv("DETENT_SCALE_SOAK_CONFIG")
	if path == "" {
		t.Skip("manual scale soak; see docs/scale-soak.md")
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
		[]string{"one tenant database; local disk object storage; no model execution", "explicit maintenance passes; entry-plane object retention sweep is a separate fixture", "percentiles are upper bounds from power-of-two microsecond histogram buckets"}}
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

func TestScaleSoakDistribution(t *testing.T) {
	for _, test := range []struct {
		name     string
		duration time.Duration
		wantMS   float64
	}{
		{"zero", 0, 0.001},
		{"fractional microsecond", 1001 * time.Nanosecond, 0.002},
		{"bucket boundary", 2048001 * time.Nanosecond, 4.096},
	} {
		t.Run(test.name, func(t *testing.T) {
			var d scaleSoakDistribution
			d.add(test.duration, true)
			got := d.summary()
			if got.Count != 1 || got.Errors != 1 || got.ErrorRate != 1 || got.P50MS != test.wantMS || got.P95MS != test.wantMS || got.P99MS != test.wantMS {
				t.Fatalf("summary=%+v want percentile=%v", got, test.wantMS)
			}
		})
	}
}

func TestScaleSoakErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name     string
		canceled bool
		err      error
		want     int64
	}{
		{"request timeout", false, context.DeadlineExceeded, 1},
		{"shutdown", true, context.Canceled, 0},
		{"protocol failure at shutdown", true, errors.New("protocol failure"), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			metrics := newScaleSoakMetrics()
			metrics.record("request", time.Now(), test.err, false, scaleSoakCanceled(ctx, test.err))
			if metrics.OperationErrors != test.want {
				t.Fatalf("operation errors=%d want=%d", metrics.OperationErrors, test.want)
			}
		})
	}
}
