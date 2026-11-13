package githuboutbound

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
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubOutboundSignal struct {
	calls chan githubcontract.OutboxItem
}

type gitHubDeadlineBackend struct {
	deadline time.Time
	bounded  bool
}

type gitHubLeaseBarrier struct {
	entered chan struct{}
	release chan struct{}
}

func (b *gitHubLeaseBarrier) Execute(context.Context, githubcontract.OutboxItem) error {
	return errors.New("step backend required")
}
func (b *gitHubLeaseBarrier) VerifyAndExecute(ctx context.Context, item githubcontract.OutboxItem) error {
	return b.Execute(ctx, item)
}
func (b *gitHubLeaseBarrier) Step(ctx context.Context, item githubcontract.OutboxItem) (githubcontract.OutboxStepResult, error) {
	if err := item.BeforeWrite(ctx); err != nil {
		return githubcontract.OutboxStepResult{}, err
	}
	close(b.entered)
	select {
	case <-b.release:
		return githubcontract.OutboxStepResult{Done: true}, nil
	case <-ctx.Done():
		return githubcontract.OutboxStepResult{}, ctx.Err()
	}
}

func (b *gitHubDeadlineBackend) Execute(ctx context.Context, _ githubcontract.OutboxItem) error {
	b.deadline, b.bounded = ctx.Deadline()
	return nil
}

func (b *gitHubDeadlineBackend) VerifyAndExecute(ctx context.Context, item githubcontract.OutboxItem) error {
	return b.Execute(ctx, item)
}

func (s githubOutboundSignal) Execute(ctx context.Context, item githubcontract.OutboxItem) error {
	select {
	case s.calls <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s githubOutboundSignal) VerifyAndExecute(ctx context.Context, item githubcontract.OutboxItem) error {
	return s.Execute(ctx, item)
}

func TestPostgresGitHubOutboundUsesConfiguredBackend(t *testing.T) {
	repository := hubtest.Repository(t)
	details := hubtest.NewDetails()
	backend := &recordingOutboxBackend{}
	deps := outboundDeps()
	deps.Repository, deps.Details, deps.Backend = repository, details, backend
	service := New(deps)
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		now := time.Now().UTC()
		raw, err := json.Marshal(githubcontract.WorkpadDesired{Phase: "summary", Summary: true, Marker: "<!-- example-summary -->", Body: "Example summary", IssueNumber: 1})
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
				return service.deps.Unavailable
			}
			return owner.EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "project", WorkItemID: "wi_example", Key: string(org), Repository: "example-org/example-project", IssueNumber: 1, Kind: string(githubcontract.MutationWorkpad), CreatedAt: now, Payload: payload})
		}); err != nil {
			t.Fatal(err)
		}
		service.deps.Organization = org
		if processed, err := service.Process(t.Context()); err != nil || !processed {
			t.Fatalf("delivery processed=%t err=%v", processed, err)
		}
		if processed, err := service.Process(t.Context()); err != nil || processed {
			t.Fatalf("completion replay processed=%t err=%v", processed, err)
		}
	}
	if backend.executeCalls != 2 || backend.effects != 2 || len(backend.deliveredKeys) != 2 {
		t.Fatalf("backend calls=%d effects=%d keys=%v", backend.executeCalls, backend.effects, backend.deliveredKeys)
	}
}

func TestGitHubOutboundCallBoundedByLease(t *testing.T) {
	service, now, _ := outboundFixture(t)
	*now = time.Now().UTC().Truncate(time.Microsecond)
	backend := &gitHubDeadlineBackend{}
	service.deps.Backend = backend
	service.deps.OutboxProcessingTimeout = time.Minute
	raw := json.RawMessage(`{"body":"Example body","marker":"<!-- example -->"}`)
	payload, err := eventstore.NewContent(service.deps.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.deps.Repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "first", WorkItemID: "wi_example", Key: "deadline", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(githubcontract.MutationWorkpad), CreatedAt: *now, Payload: payload})
	}); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.Process(t.Context()); err != nil || !processed {
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
		deps := outboundDeps()
		deps.OutboxPollInterval = 10 * time.Second
		service := New(deps)
		service.deps.Jitter = func() float64 { value := jitter[0]; jitter = jitter[1:]; return value }
		service.deps.NewTimer = func(delay time.Duration) Timer {
			waits = append(waits, delay)
			return Timer{Ticks: wake, Stop: func() {}, Reset: func(next time.Duration) { waits = append(waits, next) }}
		}
		worker := NewWorker(service)
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
	service, _, _ := outboundFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	service.deps.Now = now
	service.deps.OutboxProcessingTimeout = time.Minute
	backend := &gitHubLeaseBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	service.deps.Backend = backend
	fire := make(chan time.Time, 1)
	created, renewed := make(chan time.Duration, 1), make(chan time.Duration, 1)
	service.deps.NewTimer = func(delay time.Duration) Timer {
		created <- delay
		return Timer{Ticks: fire, Stop: func() {}, Reset: func(next time.Duration) { renewed <- next }}
	}
	payload, err := eventstore.NewContent(service.deps.Details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, json.RawMessage(`{"body":"Example"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.deps.Repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: "first", WorkItemID: "wi_example", Key: "renew", Repository: "example-org/example-project", IssueNumber: 1, Kind: string(githubcontract.MutationWorkpad), CreatedAt: now(), Payload: payload})
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
	go func() { _, err := service.Process(ctx); done <- err }()
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
	if err := service.deps.Repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
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

func TestMain(m *testing.M) { hubtest.Main(m) }
