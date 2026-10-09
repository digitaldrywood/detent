package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/devruntime"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestBoardSnapshotBrowserPreview(t *testing.T) {
	home := os.Getenv("DETENT_BOARD_SNAPSHOT_BROWSER_HOME")
	if home == "" {
		return
	}
	runtime, err := devruntime.Build(devruntime.Config{
		Home:        home,
		FixturePath: os.Getenv("DETENT_BOARD_SNAPSHOT_BROWSER_FIXTURE"),
		Port:        0,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	done := make(chan error, 1)
	go func() {
		done <- startRunningWithDependencies(ctx,
			devRuntimeBootConfig(runtime, "127.0.0.1", defaultOptions(), os.Stdout),
			startRunningDependencies{
				startupMaintenance: func(ctx context.Context, _ globalconfig.Config, _ store.Store) {
					fmt.Println("Startup hydration held")
					select {
					case <-release:
					case <-ctx.Done():
					}
				},
			})
	}()
	t.Cleanup(func() {
		cancel()
		unblock()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("isolated runtime stopped: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("timed out joining isolated runtime")
		}
	})
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if scanner.Text() != "hydrate" {
			t.Fatalf("unexpected startup control: %q", scanner.Text())
		}
		unblock()
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
