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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
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
	attempts    []scaleSoakAttempt
	payloads    []string
}

type scaleSoakAttempt struct {
	item tracker.NativeIssue
	id   string
	ref  attachment.ArchiveReference
}

func seedScaleSoak(t *testing.T, root string, config scaleSoakConfig) scaleSoakSeed {
	t.Helper()
	if err := config.validate(); err != nil {
		t.Fatal(err)
	}
	details := scaleSoakDetails{root: filepath.Join(root, "objects")}
	boardErrors := &atomic.Int64{}
	logger := slog.New(scaleSoakLogHandler{Handler: slog.DiscardHandler, boardErrors: boardErrors})
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, scaleSoakWriterTrace{})
	})
	service := openLocalPostgresHubTestService(t, repository, Config{Logger: logger, RunDetails: details, Conversation: &ConversationConfig{Enabled: true}, SecretKeys: secretTestKeys(t, "1", "1")})
	primary := tracker.OrganizationID("org_first")
	for _, organization := range []tracker.OrganizationID{primary, primary + "_second"} {
		seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-")})
		key := bytes.Repeat([]byte{0x5a}, 32)
		envelope, err := service.config.SecretKeys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			t.Fatal(err)
		}
		now := service.config.now().UTC()
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: key}); err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	service.database.hostedOrganization, service.localOrganization, service.requestMetrics.Organization = primary, string(primary), primary
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
		r.redemption.BackendIsolation = isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
		r.enroll(t)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/machines/register", r.redemption.Credential,
			map[string]any{"id": r.binding.MachineID, "hostname": "scale-host", "version": "test", "capacity": 1, "backend_isolation": r.redemption.BackendIsolation}), http.StatusOK)
		seed.runners = append(seed.runners, r)
	}
	seedScaleSoakAttempts(t, &seed, config)
	return seed
}

func seedScaleSoakAttempts(t *testing.T, seed *scaleSoakSeed, config scaleSoakConfig) {
	t.Helper()
	now := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	for index := range config.Attempts {
		item := seed.items[index%len(seed.items)]
		var f nativeFixture
		for _, candidate := range seed.fixtures {
			if candidate.project.ID == item.ProjectID {
				f = candidate
				break
			}
		}
		runner := seed.runners[0]
		for _, candidate := range seed.runners {
			if candidate.project.ID == item.ProjectID {
				runner = candidate
				break
			}
		}
		lease := nativeAttemptFixtureLease(t, runner, item, hubapi.NewID("session"))
		data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), PolicyID: hubTestPolicy().ID,
			LeaseID: lease.ID, FencingToken: lease.FencingToken, SessionID: lease.SessionID, MachineID: runner.binding.MachineID, RunnerID: runner.binding.RunnerID,
			Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}}
		var ref attachment.ArchiveReference
		for _, sequence := range []int64{1, 2, 3} {
			data.Sequence = sequence
			status, kind := "running", "run.observed"
			if sequence == 1 {
				kind = "run.started"
			}
			if sequence == 3 {
				status, kind, data.Outcome = "succeeded", "run.finished", "succeeded"
				data.CompletionBody = strings.Repeat("Example execution output. ", config.DetailBytes/25+1)[:config.DetailBytes]
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			ref, err = seed.details.StoreDetail(t.Context(), string(item.OrganizationID), raw)
			if err != nil {
				t.Fatal(err)
			}
			seed.payloads = append(seed.payloads, ref.Key)
			event := tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: item.OrganizationID, ProjectID: item.ProjectID,
				AggregateType: "issue", AggregateID: item.WorkItemID, Type: kind, SchemaVersion: 1, RecordedAt: now,
				Actor: nativeAttemptListingScope(t, f).Actor(), Data: tracker.CollaborationData{Run: &data}}
			raw, err = json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			history, err := eventstore.NewContent(seed.service.runDetailStore()).PrepareEvent(t.Context(), item.OrganizationID, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := seed.service.config.TenantRepository.WriteTenant(t.Context(), item.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				attempt, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)},
					RunID: data.RunID, Sequence: sequence, Status: status, StartedAt: now, PayloadReference: ref, PayloadObjectKey: ref.Key, RuntimeOrdered: true, WorkerCleanupRequired: new(nativeRunCleanupRequired(data))})
				if err != nil {
					return err
				}
				if err := w.SetAttemptClaimFacts(t.Context(), attemptClaimFacts(attempt, nativeAttemptClaimDisposition(data, nil, item.ProjectID), false)); err != nil {
					return err
				}
				_, err = w.AppendEvent(t.Context(), history)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := seed.service.config.TenantRepository.WriteTenant(t.Context(), item.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(lease.ID), FencingToken: int64(lease.FencingToken)}, "completed")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		admin := usagePostgresAdmin(t, seed.service)
		result, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=$1,renewed_at=$1,expires_at=$2,released_at=$1 WHERE organization_id=$3 AND lease_id=$4 AND fencing_token=$5 AND released_at IS NOT NULL`, now, now.Add(time.Hour), item.OrganizationID, lease.ID, lease.FencingToken)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			t.Fatalf("age historical lease rows=%d: %v", rows, err)
		}
		result, err = admin.ExecContext(t.Context(), `UPDATE native_attempts SET updated_at=$1 WHERE organization_id=$2 AND id=$3 AND lease_id=$4 AND fencing_token=$5 AND status='succeeded' AND sequence=3`, now, item.OrganizationID, data.AttemptID, lease.ID, lease.FencingToken)
		if err != nil {
			t.Fatal(err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			t.Fatalf("age historical attempt rows=%d: %v", rows, err)
		}
		seed.attempts = append(seed.attempts, scaleSoakAttempt{item: item, id: data.AttemptID, ref: ref})
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
	Writer          scaleSoakDistribution            `json:"write_transaction_time"`
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
		probe := &scaleSoakWriterProbe{active: map[*pgx.Conn]time.Time{}}
		c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), scaleSoakWriterKey{}, probe)))
		err := next(c)
		probe.mu.Lock()
		completed, duration, active := probe.completed, probe.duration, len(probe.active)
		probe.mu.Unlock()
		if completed > 0 {
			m.mu.Lock()
			m.Writer.add(duration, false)
			m.mu.Unlock()
		}
		if active != 0 {
			return errors.Join(err, errors.New("request left a PostgreSQL write transaction open"))
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
		now := time.Now().UTC()
		report := capacityReport(now)
		report.SharedAccountAlias = r.binding.RunnerID
		return h.post(ctx, "heartbeat", r.nativeFixture.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
			map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "scale-runner", "capacity": 1, "version": "test", "provider_reports": []any{report},
				"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: now}}, nil)
	}
	if err := heartbeat(); err != nil {
		return err
	}
	var lease tracker.NativeLease
	if err := h.post(ctx, "claim", r.nativeFixture.base+"/claims", r.redemption.Credential, providerClaim(r, item, hubapi.NewID("session")), &lease); err != nil {
		return err
	}
	event := nativeStartedEvent(lease)
	event.IdempotencyKey = hubapi.NewID("event")
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
		event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", hubapi.NewID("event"), event.Data.Sequence+1
		event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: time.Now().UTC()}
		prior := event
		event.IdempotencyKey, event.Data.Sequence = hubapi.NewID("event"), event.Data.Sequence+1
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
		{"board_refresh", func() error {
			before := seed.boardErrors.Load()
			seed.service.maintainNativeBoard(ctx)
			if seed.boardErrors.Load() != before {
				return errors.New("board refresh failed; Hub logged an error")
			}
			return ctx.Err()
		}},
		{"conversation_attachment_sweep", func() error { _, err := seed.service.conversations.SweepAttachments(ctx); return err }},
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
	organization := seed.fixtures[0].project.OrganizationID
	for _, test := range []struct {
		table string
		query string
		want  int
	}{
		{"projects", `SELECT count(*) FROM projects WHERE organization_id=$1`, 2},
		{"issues", `SELECT count(*) FROM issues WHERE organization_id=$1`, 4},
		{"native_attempts", `SELECT count(*) FROM native_attempts WHERE organization_id=$1`, 6},
		{"current attempt archive objects", `SELECT count(*) FROM native_attempts a JOIN archive_objects o ON o.organization_id=a.organization_id AND o.object_key=a.payload_object_key WHERE a.organization_id=$1`, 6},
		{"runner_identities", `SELECT count(*) FROM runner_identities WHERE organization_id=$1`, 2},
	} {
		var count int
		if err := usagePostgresAdmin(t, seed.service).QueryRowContext(t.Context(), test.query, organization).Scan(&count); err != nil || count != test.want {
			t.Fatalf("%s count=%d want=%d: %v", test.table, count, test.want, err)
		}
	}
	var payloads int
	if err := usagePostgresAdmin(t, seed.service).QueryRowContext(t.Context(), `SELECT count(*) FROM archive_objects WHERE organization_id=$1 AND object_key=ANY($2::text[])`, organization, seed.payloads).Scan(&payloads); err != nil || payloads != 18 {
		t.Fatalf("retained attempt payloads=%d, want 18: %v", payloads, err)
	}
	seen := map[tracker.ProjectID]bool{}
	for _, row := range seed.attempts {
		var f nativeFixture
		for _, candidate := range seed.fixtures {
			if candidate.project.ID == row.item.ProjectID {
				f = candidate
				break
			}
		}
		if err := seed.service.config.TenantRepository.ReadTenant(t.Context(), row.item.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(attemptArchiveReader)
			if !ok {
				return hubapi.NativeRunEventStorageUnavailable()
			}
			object, err := owner.ArchiveObjectByKey(t.Context(), row.ref.Key)
			if err != nil {
				return err
			}
			if object.Reference != row.ref || object.Reference.Validate(string(row.item.OrganizationID)) != nil {
				return artifact.ErrIntegrity
			}
			return nil
		}); err != nil {
			t.Fatalf("missing detail: %v", err)
		}
		data, err := seed.service.readNativeAttemptTenant(t.Context(), nativeAttemptListingScope(t, f), string(row.item.WorkItemID), row.id)
		if err != nil || len(data.CompletionBody) < config.DetailBytes {
			t.Fatalf("unreadable detail: %v", err)
		}
		if data.Sequence != 3 || data.Status != "succeeded" {
			t.Fatalf("seeded attempt sequence=%d status=%s", data.Sequence, data.Status)
		}
		seen[row.item.ProjectID] = true
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
	if err := usagePostgresAdmin(t, seed.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND status='running' AND sequence=5", seed.fixtures[0].project.OrganizationID).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("live attempts=%d: %v", attempts, err)
	}
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

type scaleSoakWriterKey struct{}
type scaleSoakBeginKey struct{}
type scaleSoakWriterProbe struct {
	mu        sync.Mutex
	active    map[*pgx.Conn]time.Time
	duration  time.Duration
	completed int
}

func (p *scaleSoakWriterProbe) begin(conn *pgx.Conn, started time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active[conn] = started
}

func (p *scaleSoakWriterProbe) end(conn *pgx.Conn) {
	if conn.PgConn().TxStatus() != 'I' {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if started, ok := p.active[conn]; ok {
		p.duration += time.Since(started)
		p.completed++
		delete(p.active, conn)
	}
}

func scaleSoakWriteBegin(sql string) bool {
	statement := strings.ToUpper(strings.TrimSpace(sql))
	return strings.HasPrefix(statement, "BEGIN") && !strings.Contains(statement, "READ ONLY")
}

type scaleSoakWriterTrace struct{}

func (scaleSoakWriterTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if scaleSoakWriteBegin(data.SQL) {
		return context.WithValue(ctx, scaleSoakBeginKey{}, time.Now())
	}
	return ctx
}

func (scaleSoakWriterTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if probe, ok := ctx.Value(scaleSoakWriterKey{}).(*scaleSoakWriterProbe); ok {
		if started, ok := ctx.Value(scaleSoakBeginKey{}).(time.Time); ok && data.Err == nil && data.CommandTag.String() == "BEGIN" {
			probe.begin(conn, started)
		}
		probe.end(conn)
	}
}

func (scaleSoakWriterTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		if scaleSoakWriteBegin(query.SQL) {
			return context.WithValue(ctx, scaleSoakBeginKey{}, time.Now())
		}
	}
	return ctx
}

func (scaleSoakWriterTrace) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	if probe, ok := ctx.Value(scaleSoakWriterKey{}).(*scaleSoakWriterProbe); ok && scaleSoakWriteBegin(data.SQL) && data.Err == nil {
		if started, ok := ctx.Value(scaleSoakBeginKey{}).(time.Time); ok {
			probe.begin(conn, started)
		}
	}
}

func (scaleSoakWriterTrace) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	if probe, ok := ctx.Value(scaleSoakWriterKey{}).(*scaleSoakWriterProbe); ok {
		probe.end(conn)
	}
}
