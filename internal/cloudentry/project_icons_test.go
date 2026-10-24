package cloudentry

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func TestProjectIconAttachmentLifecycle(t *testing.T) {
	t.Parallel()
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f := newEntryFixtureWithLogger(t, slog.New(slog.DiscardHandler), func(config *hubserver.Config) { config.Hosted.SharedEntry.Icons = attachment.Icons{Storage: storage} })
	f.service.attachments = storage
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, page := alice.get("/organizations/org_alpha/organization")
	csrf := csrfFrom(t, page)
	project := attachmentProject(t, alice, "org_alpha")
	base := "/api/v2/organizations/org_alpha/projects/" + project
	get := attachmentRequest(t, alice, http.MethodGet, base+"/integration", nil, nil)
	var integration hubserver.ProjectIntegration
	if err := json.Unmarshal(get.Body.Bytes(), &integration); err != nil || get.Code != 200 {
		t.Fatalf("integration %d %s %v", get.Code, get.Body.String(), err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.Set(x, y, color.NRGBA{R: byte(x * 3), G: byte(y * 3), B: 100, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	save := func(kind string) hubserver.ProjectIntegration {
		t.Helper()
		body := map[string]any{"icon": map[string]string{"kind": kind}, "expected_revision": fmt.Sprint(integration.Revision), "idempotency_key": fmt.Sprint("icon-", integration.Revision)}
		if kind == "image" {
			body["content_base64"] = buffer.Bytes()
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response := attachmentRequest(t, alice, http.MethodPut, base+"/icon", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
		if response.Code != 200 {
			t.Fatalf("icon save %d %s", response.Code, response.Body.String())
		}
		var saved hubserver.ProjectIntegration
		if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
			t.Fatal(err)
		}
		integration = saved
		return saved
	}
	first := save("image")
	read := attachmentRequest(t, alice, http.MethodGet, first.Icon.URL, nil, nil)
	config, err := png.DecodeConfig(bytes.NewReader(read.Body.Bytes()))
	if read.Code != 200 || err != nil || config.Width != 128 || config.Height != 128 {
		t.Fatalf("proxy output %d %+v %v", read.Code, config, err)
	}
	if got := attachmentRequest(t, newBrowser(t, f.service.Handler()), http.MethodGet, first.Icon.URL, nil, nil); got.Code != 404 && got.Code != 401 {
		t.Fatalf("anonymous read=%d", got.Code)
	}
	foreign := strings.Replace(first.Icon.URL, "org_alpha", "org_beta", 1)
	if got := attachmentRequest(t, alice, http.MethodGet, foreign, nil, nil); got.Code != 404 {
		t.Fatalf("foreign read=%d", got.Code)
	}
	if got := attachmentRequest(t, alice, http.MethodDelete, first.Icon.URL, nil, map[string]string{"X-CSRF-Token": csrf}); got.Code != 404 {
		t.Fatalf("active icon delete=%d %s", got.Code, got.Body.String())
	}
	db, err := sql.Open("sqlite", f.fixtures["org_alpha"].path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	used := func() int64 {
		t.Helper()
		var n int64
		if err := db.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	second := save("image")
	for _, test := range []struct {
		saved hubserver.ProjectIntegration
		reset bool
	}{{first, false}, {second, true}} {
		saved := test.saved
		if test.reset {
			save("automatic")
		}
		if got := attachmentRequest(t, alice, http.MethodGet, saved.Icon.URL, nil, nil); got.Code != 404 {
			t.Fatalf("replaced icon readable: %d", got.Code)
		}
		before := used()
		organization, err := f.service.readyOrganization(t.Context(), "org_alpha")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
			t.Fatal(err)
		}
		key, err := attachment.Key("org_alpha", saved.Icon.ImageKey)
		if err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		_, exists := store.objects[key]
		store.mu.Unlock()
		if exists {
			t.Fatal("replaced/reset icon object remains")
		}
		if after := used(); before-after != int64(read.Body.Len()) {
			t.Fatalf("storage release=%d, want %d", before-after, read.Body.Len())
		}
	}
	if integration.Icon != nil {
		t.Fatalf("reset icon=%+v", integration.Icon)
	}
}
