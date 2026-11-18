package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func TestAttachCommand(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusCreated, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence.txt")
			if err := os.WriteFile(path, []byte("test evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			record := attachment.Metadata{ID: "att_0123456789abcdef0123456789abcdef", ProjectID: "prj_cli", Name: "evidence.txt", ContentType: "text/plain", Size: 13, SHA256: strings.Repeat("a", 64)}
			record.Reference = record.Markdown("org_cli")
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				content, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPost || r.URL.Path != "/organizations/org_cli/api/v2/projects/prj_cli/attachments" || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Attachment-Name") != "evidence.txt" || string(content) != "test evidence" {
					t.Errorf("unexpected upload: %s %s %q", r.Method, r.URL.Path, content)
				}
				w.WriteHeader(status)
				if status == http.StatusCreated {
					if err := json.NewEncoder(w).Encode(record); err != nil {
						t.Error(err)
					}
				} else if _, err := io.WriteString(w, `{"code":"not_found","message":"Resource was not found"}`); err != nil {
					t.Error(err)
				}
			})
			httpClient := &http.Client{Transport: readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				return recorder.Result(), nil
			})}
			var output bytes.Buffer
			cmd := newAttachCommand(func(key string) string {
				if key == "DETENT_HUB_TOKEN" {
					return "test-token"
				}
				return ""
			}, httpClient)
			cmd.SilenceUsage = true
			cmd.SetOut(&output)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{path, "--project", "prj_cli", "--organization", "org_cli", "--hub-url", "https://cloud.example/organizations/org_cli"})
			err := cmd.Execute()
			if status == http.StatusCreated {
				if err != nil || output.String() != record.Reference+"\n" {
					t.Fatalf("output=%q error=%v", output.String(), err)
				}
			} else if err == nil || output.Len() != 0 {
				t.Fatalf("refusal output=%q error=%v", output.String(), err)
			}
		})
	}
}
