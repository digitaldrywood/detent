package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
)

func TestArtifactJournalReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, scenario := range []string{"lost reply", "restart after short chunk", "unicode boundary", "disconnected"} {
		t.Run(scenario, func(t *testing.T) {
			directory := t.TempDir()
			input := strings.Repeat("x", 64<<10) + "tail"
			if scenario == "unicode boundary" {
				input = strings.Repeat("x", (64<<10)-1) + "🌲tail"
			}
			if scenario == "restart after short chunk" {
				input = "short"
			}
			var parts []artifact.Part
			lose, down := scenario == "lost reply", scenario == "disconnected"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if down {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"code":"storage_unreachable"}`))
					return
				}
				if r.URL.Path == "/v1/uploads" {
					_ = json.NewEncoder(w).Encode(artifact.Upload{ArtifactID: "artifact_" + strings.Repeat("a", 32), State: "uploading"})
					return
				}
				var p artifact.Part
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
					w.WriteHeader(422)
					return
				}
				if p.Sequence < len(parts) {
					if !bytes.Equal(parts[p.Sequence].Data, p.Data) {
						t.Error("retry changed chunk")
						w.WriteHeader(409)
						return
					}
				} else if p.Sequence == len(parts) {
					parts = append(parts, p)
				} else {
					t.Error("chunk gap")
				}
				if lose {
					lose = false
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"code":"storage_unreachable"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(artifact.Object{})
			}))
			defer server.Close()
			newExecution := func() *nativeExecution {
				return &nativeExecution{artifacts: nativeArtifacts{directory: directory, client: &artifact.Client{Origin: server.URL, Token: func(_ context.Context) (string, error) { return "worker", nil }}, reservation: artifact.Reservation{Bytes: 4 << 20}}}
			}
			e := newExecution()
			err := e.ArtifactLog(t.Context(), input)
			if scenario == "lost reply" || scenario == "disconnected" {
				if !errors.Is(err, artifact.ErrStorage) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if scenario == "restart after short chunk" {
				if err := e.artifacts.flush(t.Context(), true); err != nil {
					t.Fatal(err)
				}
			}
			down = false
			e = newExecution()
			if scenario == "restart after short chunk" {
				if err := e.ArtifactLog(t.Context(), " after restart"); err != nil {
					t.Fatal(err)
				}
				input += " after restart"
			}
			if err := e.artifacts.flush(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			var got []byte
			for index, p := range parts {
				if p.Sequence != index || p.SHA256 != artifact.Digest(p.Data) || !utf8.Valid(p.Data) {
					t.Fatal("invalid part", index)
				}
				frozen, err := os.ReadFile(filepath.Join(directory, "part-"+strconv.Itoa(index)))
				if err != nil || !bytes.Equal(frozen, p.Data) {
					t.Fatal("chunk journal", err)
				}
				got = append(got, p.Data...)
			}
			if string(got) != input {
				t.Fatal("journal replay lost or duplicated bytes")
			}
		})
	}
}

func TestArtifactJournalRejectsInvalidAndOversizeLogs(t *testing.T) {
	t.Parallel()
	for _, delta := range []string{string([]byte{255}), strings.Repeat("x", 1025)} {
		t.Run(strconv.Itoa(len(delta)), func(t *testing.T) {
			e := &nativeExecution{artifacts: nativeArtifacts{directory: t.TempDir(), reservation: artifact.Reservation{Bytes: artifact.MaxManifestBytes + 1024}}}
			if err := e.ArtifactLog(t.Context(), delta); err == nil || !e.artifacts.incomplete {
				t.Fatal("invalid log accepted", err)
			}
		})
	}
}

func TestArtifactFinalizationReconcilesLostAcknowledgment(t *testing.T) {
	for _, stage := range []string{"reserve", "append", "finalize log", "finalize diff"} {
		t.Run(stage, func(t *testing.T) {
			states := map[string]string{}
			parts := map[string][]artifact.Part{}
			lost := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current := ""
				var response any
				if r.URL.Path == "/v1/uploads" {
					var reservation artifact.Reservation
					if err := json.NewDecoder(r.Body).Decode(&reservation); err != nil {
						t.Error(err)
						return
					}
					id := "artifact_" + strings.Repeat("a", 32)
					if reservation.Kind == "diff" {
						id = "artifact_" + strings.Repeat("b", 32)
					}
					if states[id] == "" {
						states[id] = "uploading"
					}
					response = artifact.Upload{ArtifactID: id, State: states[id]}
					current = "reserve"
				} else {
					id := strings.Split(r.URL.Path, "/")[3]
					if strings.HasSuffix(r.URL.Path, "/parts") {
						var part artifact.Part
						if err := json.NewDecoder(r.Body).Decode(&part); err != nil {
							t.Error(err)
							return
						}
						if states[id] != "uploading" {
							t.Error("append after committed finalization")
						}
						if part.Sequence == len(parts[id]) {
							parts[id] = append(parts[id], part)
						} else if part.Sequence >= len(parts[id]) || !bytes.Equal(parts[id][part.Sequence].Data, part.Data) {
							t.Error("replay changed bytes")
						}
						response = artifact.Object{}
						current = "append"
					} else {
						states[id] = "complete"
						response = artifact.Reference{}
						current = "finalize log"
						if strings.Contains(id, "bbbb") {
							current = "finalize diff"
						}
					}
				}
				if current == stage && !lost {
					lost = true
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"code":"storage_unreachable"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "log"), []byte("finished work"), 0600); err != nil {
				t.Fatal(err)
			}
			e := &nativeExecution{artifacts: nativeArtifacts{directory: directory, base: strings.Repeat("a", 40), client: &artifact.Client{Origin: server.URL, Transport: server.Client().Transport, Token: func(context.Context) (string, error) { return "worker", nil }}, reservation: artifact.Reservation{Kind: "log", Key: "attempt:log", Bytes: 4 << 20}, capture: func(context.Context, string, string) (artifact.GitCapture, error) { return artifact.GitCapture{}, nil }}}
			if err := e.FinalizeArtifacts(t.Context(), directory); !errors.Is(err, artifact.ErrStorage) {
				t.Fatalf("missing injected failure: %v", err)
			}
			if e.artifacts.finished {
				t.Fatal("uncertain upload marked finished")
			}
			if err := e.FinalizeArtifacts(t.Context(), directory); err != nil {
				t.Fatal(err)
			}
			if !e.artifacts.finished || len(parts["artifact_"+strings.Repeat("a", 32)]) != 1 {
				t.Fatal("retry lost finished work")
			}
		})
	}
}
