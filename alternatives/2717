package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// spriteAPISocket is the Fly Sprite management socket. A Sprite pauses about
// 30 seconds after its last activity, and an outbound Hub connection does not
// count, so a job holds a Sprite task for its whole duration.
const spriteAPISocket = "/.sprite/api.sock"

const (
	spriteTaskExpiry  = "5m"
	spriteTaskRefresh = time.Minute
)

var spriteTaskSequence atomic.Uint64

func spriteSocketPresent() bool {
	info, err := os.Stat(spriteAPISocket)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSocket != 0
}

func holdSpriteTask(ctx context.Context, failed func()) (func(), error) {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", spriteAPISocket)
	}
	return holdSpriteTaskWith(ctx, dial, spriteTaskRefresh, failed)
}

func holdSpriteTaskWith(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), refresh time.Duration, failed func()) (func(), error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}}
	name := fmt.Sprintf("detent-%d-%d", os.Getpid(), spriteTaskSequence.Add(1))
	call := func(ctx context.Context, method, path, body string) error {
		request, err := http.NewRequestWithContext(ctx, method, "http://sprite"+path, bytes.NewBufferString(body))
		if err != nil {
			return err
		}
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return fmt.Errorf("sprite task %s %s: status %d", method, path, response.StatusCode)
		}
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := call(ctx, http.MethodPost, "/v1/tasks", fmt.Sprintf(`{"name":%q,"expire":%q}`, name, spriteTaskExpiry)); err != nil {
		return nil, err
	}
	held, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(refresh)
		defer ticker.Stop()
		for {
			select {
			case <-held.Done():
				return
			case <-ticker.C:
				if err := call(held, http.MethodPut, "/v1/tasks/"+name, fmt.Sprintf(`{"expire":%q}`, spriteTaskExpiry)); err != nil && held.Err() == nil && failed != nil {
					once.Do(failed)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		if err := call(release, http.MethodDelete, "/v1/tasks/"+name, ""); err != nil {
			slog.Warn("sprite task could not be released", "error", err)
		}
	}, nil
}

// SpriteSocketPresent reports whether this process runs inside a Fly Sprite.
func SpriteSocketPresent() bool {
	return spriteSocketPresent()
}

// HoldSpriteTask keeps the Sprite awake until the returned release runs. The
// failed callback reports a refresh that the Sprite refused.
func HoldSpriteTask(ctx context.Context, failed func()) (func(), error) {
	return holdSpriteTask(ctx, failed)
}
