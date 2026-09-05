package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/visualreview"
)

func TestHostedVisualReviewManifestMapsToCaptureMediaRoute(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"assets":[{"id":"desktop","path":"media/original.png"}]}`)
	got, err := hostedVisualReviewManifest(raw, []visualreview.Asset{{ID: "desktop", StorageKey: "owned/hash.png"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"path":"media/desktop.png"`) {
		t.Fatalf("hosted manifest = %s", got)
	}
}

func TestVisualReviewStoragePathRejectsEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, ok := visualReviewStoragePath(root, "../secret"); ok {
		t.Fatal("traversal path accepted")
	}
	if got, ok := visualReviewStoragePath(root, "capture/image.png"); !ok || !strings.HasPrefix(got, root) {
		t.Fatalf("safe path = %q, %t", got, ok)
	}
}

func TestInspectVisualReviewMediaRejectsArbitraryVideo(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fake.mp4")
	if err := os.WriteFile(path, []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := inspectVisualReviewMedia(path, "video"); err == nil {
		t.Fatal("arbitrary bytes accepted as video")
	}
}

func TestVisualReviewProjectAllowedRejectsScopedKey(t *testing.T) {
	t.Parallel()
	e := echo.New()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	context := e.NewContext(request, recorder)
	server := &Server{}
	server.setAPICredential(context, apikey.Credential{ProjectIDs: []string{"alpha"}})
	if err := visualReviewProjectAllowed(context, "beta"); err == nil {
		t.Fatal("cross-project read accepted")
	}
}

func TestVisualReviewMutationRequiresTokenOrSameOrigin(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "http://detent.test/api", nil)
	request.Host = "detent.test"
	if visualReviewMutationSourceAllowed(request) {
		t.Fatal("unattributed mutation accepted")
	}
	request.Header.Set("Origin", "http://detent.test")
	if !visualReviewMutationSourceAllowed(request) {
		t.Fatal("same-origin mutation rejected")
	}
}
