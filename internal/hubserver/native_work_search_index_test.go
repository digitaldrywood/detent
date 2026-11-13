package hubserver

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
)

func TestSearchRefreshRunsAtJitteredOffset(t *testing.T) {
	offsets := map[float64]time.Duration{}
	for _, jitter := range []float64{0.2, 0.7} {
		service := &Service{config: Config{WebhookMaintenanceInterval: time.Minute, deps: hubdeps.Deps{
			Jitter: func() float64 { return jitter },
			NewMaintenanceTimer: func(delay time.Duration) hubdeps.Timer {
				offsets[jitter] = delay
				return hubdeps.Timer{Reset: func(time.Duration) {}, Stop: func() {}}
			},
		}}}
		service.searchRefreshTimer()
	}
	if offsets[0.2] == offsets[0.7] || offsets[0.2] < 10*time.Second || offsets[0.7] < 40*time.Second || offsets[0.7] >= time.Minute {
		t.Fatalf("search refresh offsets=%v", offsets)
	}
	if searchstore.RefreshTickDocuments != 100 || searchstore.RefreshTickDuration != 2*time.Second {
		t.Fatalf("per-tick budget changed: documents=%d duration=%s", searchstore.RefreshTickDocuments, searchstore.RefreshTickDuration)
	}
}

func TestSearchMaintenanceDeadlineCancelsBlockedIdentityRefresh(t *testing.T) {
	t.Parallel()
	objects := &memoryRunDetails{}
	expired := make(chan context.CancelFunc, 1)
	service := newPostgresNativeService(t, Config{RunDetails: objects, deps: hubdeps.Deps{SearchRefreshTimeout: func(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
		if budget != searchstore.RefreshTickDuration {
			t.Errorf("tick budget=%s want %s", budget, searchstore.RefreshTickDuration)
		}
		ctx, cancel := context.WithCancel(parent)
		select {
		case expired <- cancel:
		default:
			t.Error("tick deadline created more than once")
		}
		return ctx, cancel
	}}})
	f := newNativeFixture(t, service, "", "Example project")
	f.create(t, "identity refresh blocked")
	if _, err := nativeAttemptTestDatabase(t, f).ExecContext(t.Context(), "UPDATE issues SET doctor_identity_revision=NULL WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	parent := t.Context()
	if end, ok := t.Deadline(); ok {
		guarded, cancel := context.WithDeadline(parent, end.Add(-time.Until(end)/10))
		t.Cleanup(cancel)
		parent = guarded
	}
	read := make(chan struct{})
	var readOnce sync.Once
	stop, helperDone := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-helperDone
	})
	objects.beforeRead = func(ctx context.Context) error {
		readOnce.Do(func() { close(read) })
		<-ctx.Done()
		return ctx.Err()
	}
	go func() {
		defer close(helperDone)
		select {
		case <-read:
		case <-stop:
			return
		}
		select {
		case expire := <-expired:
			expire()
		case <-stop:
		}
	}()
	service.maintainNativeSearch(parent)
	objects.beforeRead = nil
	select {
	case <-read:
	default:
		t.Fatal("identity refresh did not reach its object read")
	}
	if parent.Err() != nil {
		t.Fatal("blocked identity refresh outlived the tick deadline")
	}
	if more, err := service.refreshNativeIdentity(t.Context()); err != nil || !more {
		t.Fatalf("identity refresh after expired tick more=%v err=%v", more, err)
	}
}
