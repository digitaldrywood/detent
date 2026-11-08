package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubOutboundSignal struct{ calls chan OutboxItem }

type gitHubDeadlineBackend struct {
	deadline time.Time
	bounded  bool
}

type gitHubLeaseBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *gitHubLeaseBarrier) Execute(context.Context, OutboxItem) error {
	return errors.New("step backend required")
}
func (b *gitHubLeaseBarrier) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return b.Execute(ctx, item)
}
func (b *gitHubLeaseBarrier) Step(ctx context.Context, item OutboxItem) (OutboxStepResult, error) {
	if err := item.BeforeWrite(ctx); err != nil {
		return OutboxStepResult{}, err
	}
	close(b.entered)
	select {
	case <-b.release:
		return OutboxStepResult{Done: true}, nil
	case <-ctx.Done():
		return OutboxStepResult{}, ctx.Err()
	}
}

func (b *gitHubDeadlineBackend) Execute(ctx context.Context, _ OutboxItem) error {
	b.deadline, b.bounded = ctx.Deadline()
	return nil
}

func (b *gitHubDeadlineBackend) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return b.Execute(ctx, item)
}

func (s githubOutboundSignal) Execute(ctx context.Context, item OutboxItem) error {
	select {
	case s.calls <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s githubOutboundSignal) VerifyAndExecute(ctx context.Context, item OutboxItem) error {
	return s.Execute(ctx, item)
}

func TestPostgresGitHubOutboundWorkerStarts(t *testing.T) {
	calls := make(chan OutboxItem, 1)
	service := openTestService(t, Config{OutboxBackend: githubOutboundSignal{calls: calls}})
	org := service.database.hostedOrganization
	raw := json.RawMessage(`{"phase":"summary","body":"Example summary","marker":"<!-- example-summary -->"}`)
	payload, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}); err != nil {
			return err
		}
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return ErrPostgresFeatureUnavailable
		}
		return owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: "explicit", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: time.Now(), Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-calls:
		var desired WorkpadDesired
		if err := json.Unmarshal(item.Desired, &desired); err != nil {
			t.Fatal(err)
		}
		if item.IdempotencyKey != "explicit" || desired.Body != "Example summary" || desired.Marker != "<!-- example-summary -->" {
			t.Fatalf("delivered=%+v", item)
		}
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

func TestPostgresGitHubOutboundUsesConfiguredBackend(t *testing.T) {
	repository := projectRecordPostgres(t)
	details := &memoryRunDetails{}
	backend := &recordingOutboxBackend{}
	service := &Service{database: &database{}, config: (Config{TenantRepository: repository, RunDetails: details, OutboxBackend: backend}).normalized()}
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		now := time.Now().UTC()
		raw, err := json.Marshal(WorkpadDesired{Phase: "summary", Summary: true, Marker: "<!-- example-summary -->", Body: "Example summary", IssueNumber: 1})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "project", Name: "Example", Profile: "native"}); err != nil {
				return err
			}
			owner, ok := w.(projectstore.GitHubOutboxWriter)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			return owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: string(org), Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: now, Payload: payload})
		}); err != nil {
			t.Fatal(err)
		}
		service.database.hostedOrganization = org
		if processed, err := service.ProcessOutbox(t.Context()); err != nil || !processed {
			t.Fatalf("delivery processed=%t err=%v", processed, err)
		}
		if processed, err := service.ProcessOutbox(t.Context()); err != nil || processed {
			t.Fatalf("completion replay processed=%t err=%v", processed, err)
		}
	}
	if backend.executeCalls != 2 || backend.effects != 2 || len(backend.deliveredKeys) != 2 {
		t.Fatalf("backend calls=%d effects=%d keys=%v", backend.executeCalls, backend.effects, backend.deliveredKeys)
	}
}

func TestGitHubOutboundCallBoundedByLease(t *testing.T) {
	service, now, _ := gitHubWebhookRecordFixture(t)
	*now = time.Now().UTC().Truncate(time.Microsecond)
	backend := &gitHubDeadlineBackend{}
	service.config.OutboxBackend = backend
	service.config.OutboxProcessingTimeout = time.Minute
	raw := json.RawMessage(`{"body":"Example body","marker":"<!-- example -->"}`)
	payload, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "first", WorkItemID: "wi_example", Key: "deadline", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: *now, Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ProcessOutbox(t.Context()); err != nil || !processed {
		t.Fatalf("processed=%t err=%v", processed, err)
	}
	if !backend.bounded || backend.deadline.After(now.Add(time.Minute)) {
		t.Fatalf("external context bounded=%t deadline=%v lease=%v", backend.bounded, backend.deadline, now.Add(time.Minute))
	}
}

func TestGitHubOutboundPollJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wake := make(chan time.Time)
		var waits []time.Duration
		jitter := []float64{0, 0.5, 1}
		service := &Service{config: (Config{OutboxPollInterval: 10 * time.Second}).normalized()}
		service.config.jitter = func() float64 { value := jitter[0]; jitter = jitter[1:]; return value }
		service.config.newOutboxTimer = func(delay time.Duration) outboxTimer {
			waits = append(waits, delay)
			return outboxTimer{ticks: wake, stop: func() {}, reset: func(next time.Duration) { waits = append(waits, next) }}
		}
		worker := newOutboxWorker(service)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() { defer close(done); worker.run(ctx) }()
		for index, want := range []time.Duration{5 * time.Second, 7500 * time.Millisecond, 10 * time.Second} {
			synctest.Wait()
			if len(waits) != index+1 || waits[index] != want {
				t.Fatalf("poll waits=%v want next=%v", waits, want)
			}
			if index < 2 {
				wake <- time.Now()
			}
		}
	})
}

func TestGitHubOutboundRenewsExecutingLease(t *testing.T) {
	service, _, _ := gitHubWebhookRecordFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	service.config.now = now
	service.config.OutboxProcessingTimeout = time.Minute
	backend := &gitHubLeaseBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	service.config.OutboxBackend = backend
	fire := make(chan time.Time, 1)
	created, renewed := make(chan time.Duration, 1), make(chan time.Duration, 1)
	service.config.newOutboxTimer = func(delay time.Duration) outboxTimer {
		created <- delay
		return outboxTimer{ticks: fire, stop: func() {}, reset: func(next time.Duration) { renewed <- next }}
	}
	payload, err := eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, json.RawMessage(`{"body":"Example"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "first", WorkItemID: "wi_example", Key: "renew", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: now(), Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := false
	var release sync.Once
	defer func() {
		cancel()
		release.Do(func() { close(backend.release) })
		if !joined {
			<-done
		}
	}()
	go func() { _, err := service.ProcessOutbox(ctx); done <- err }()
	select {
	case <-backend.entered:
	case err := <-done:
		joined = true
		t.Fatalf("start effect: %v", err)
	}
	if delay := <-created; delay != 20*time.Second {
		t.Fatalf("renewal period=%v", delay)
	}
	clock.Add(int64(20 * time.Second))
	fire <- now()
	select {
	case <-renewed:
	case err := <-done:
		joined = true
		t.Fatalf("renew lease: %v", err)
	}
	clock.Add(int64(40 * time.Second))
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), now(), now().Add(time.Minute), "replacement")
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("renewed execution was reclaimed at original expiry: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(backend.release) })
	if err := <-done; err != nil {
		t.Error(err)
	}
	joined = true
}
