package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/instancelock"

	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func TestApplyRunningUpdate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		status           int
		wantErr          bool
		legacy           bool
		count            int
		large            bool
		ignoreProjection bool
	}{
		{"accepted", http.StatusAccepted, false, false, 2, false, false},
		{"large fleet", http.StatusAccepted, false, false, 202, false, false},
		{"oversized fleet state", http.StatusAccepted, false, false, 202, true, false},
		{"old large instance reports failed pre-check", 0, true, false, 202, true, true},
		{"authorization denied", http.StatusForbidden, true, false, 2, false, false},
		{"unavailable", http.StatusServiceUnavailable, true, false, 2, false, false},
		{"old instance", http.StatusServiceUnavailable, true, true, 2, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reported := make(chan struct{})
			var output bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("missing credential")
				}
				if r.Method == http.MethodGet {
					if test.legacy {
						io.WriteString(w, `{"running":[{},{}]}`)
					} else {
						state := map[string]any{"running": make([]struct{}, test.count), "counts": map[string]int{"running": test.count}, "update": map[string]int{"active_attempts": test.count}}
						if test.large {
							if r.URL.Query().Get("fields") == "update,counts" && !test.ignoreProjection {
								delete(state, "running")
							} else {
								state["board_issues"] = strings.Repeat("x", 1<<20)
							}
						}
						if err := json.NewEncoder(w).Encode(state); err != nil {
							t.Error(err)
						}
					}
					return
				}
				if test.legacy || test.ignoreProjection {
					t.Error("sent update to legacy instance")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var request struct {
					Confirm, Release bool
					FromRelease      bool `json:"from_release"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if !request.Confirm || !request.Release || !request.FromRelease {
					t.Errorf("request = %+v", request)
				}
				// Ensure the CLI can report state while apply is waiting.
				<-reported
				w.WriteHeader(test.status)
				if test.wantErr {
					io.WriteString(w, `{"error":{"message":"fixture refusal"}}`)
					return
				}
				io.WriteString(w, `{"action":"updated","latest_version":"1.2.4"}`)
			}))
			defer server.Close()
			address, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &DashboardReadClient{baseURL: address, credential: "fixture", http: server.Client()}
			writer := &drainReportWriter{Writer: &output, reported: reported}
			status, err := client.applyRunningUpdate(context.Background(), detentupdate.ApplyOptions{AssumeYes: true, FromRelease: true, Stderr: writer})
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v", err)
			}
			if test.ignoreProjection && !strings.Contains(err.Error(), "check running Detent update coordination: read dashboard API response: response exceeds 1048576 bytes") {
				t.Fatalf("pre-check error = %v", err)
			}
			if !test.wantErr && status.Action != detentupdate.ActionUpdated {
				t.Fatalf("status = %+v", status)
			}
			if !test.legacy && !test.ignoreProjection && !strings.Contains(output.String(), "draining for update: "+strconv.Itoa(test.count)+" active attempts") {
				t.Fatalf("output = %q", output.String())
			}
		})
	}
}

type drainReportWriter struct {
	io.Writer
	reported chan struct{}
}

func (w *drainReportWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	select {
	case <-w.reported:
	default:
		close(w.reported)
	}
	return n, err
}

func TestReportDrainPreservesLegacyRestartProgress(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/state" {
					t.Errorf("path = %q", r.URL.Path)
				}
				if err := json.NewEncoder(w).Encode(map[string]any{"running": make([]struct{}, count)}); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			address, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &DashboardReadClient{baseURL: address, http: server.Client()}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reported := make(chan struct{})
			var output bytes.Buffer
			stop := client.reportDrain(ctx, &drainReportWriter{Writer: &output, reported: reported}, "restart")
			defer stop()
			select {
			case <-reported:
			case <-ctx.Done():
				t.Fatalf("restart progress not reported: %v", ctx.Err())
			}
			if want := "draining for restart: " + strconv.Itoa(count) + " active attempts"; !strings.Contains(output.String(), want) {
				t.Fatalf("output = %q, want %q", output.String(), want)
			}
		})
	}
}

func TestApplyRunningUpdateRequiresLiveInstanceCoordination(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{false, true} {
		t.Run(strconv.FormatBool(held), func(t *testing.T) {
			root := t.TempDir()
			if held {
				lock, err := instancelock.Acquire(filepath.Join(root, "detent.db.lock"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := lock.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			cmd := &cobra.Command{}
			cmd.Flags().String("config", filepath.Join(root, "global.yaml"), "")
			_, handled, err := ApplyRunningUpdate(t.Context(), cmd, detentupdate.ApplyOptions{})
			if handled != held || (err != nil) != held {
				t.Fatalf("handled=%t error=%v", handled, err)
			}
		})
	}
}
