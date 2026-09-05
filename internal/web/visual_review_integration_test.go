package web_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/telemetry"
	web "github.com/digitaldrywood/detent/internal/web"
)

func TestVisualReviewManualHarness(t *testing.T) {
	if os.Getenv("DETENT_VISUAL_REVIEW_HARNESS") != "1" {
		t.Skip("set DETENT_VISUAL_REVIEW_HARNESS=1 for manual browser review")
	}
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue := telemetry.Issue{ID: "issue-ui", Identifier: "digitaldrywood/detent#42", ProjectID: "detent", Title: "Isolated visual review fixture", State: "Human Review", PullRequest: &telemetry.PullRequest{Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42", HeadSHA: head}}
	if err := deps.Hub.Publish(telemetry.Snapshot{Project: telemetry.Project{ID: "detent"}, BoardIssues: []telemetry.Issue{issue}}); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewServer(web.Config{RuntimeDBPath: t.TempDir() + "/runtime.db", VisualReviewMediaDir: t.TempDir(), ServerAddress: "127.0.0.1:0", GlobalConfig: globalconfig.Config{APIToken: "review-token"}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := visualReviewHarnessPNG(t)
	response := visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipart(t, visualReviewManifest(t, "manual-round", head, pngBytes), pngBytes), "review-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("fixture import: %s", response.Body.String())
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	t.Logf("VISUAL_REVIEW_URL=%s/projects/detent/visual-reviews/manual-round/", httpServer.URL)
	t.Logf("HUMAN_REVIEW_SHEET_URL=%s/api/v1/board/card?project=detent&issue=issue-ui", httpServer.URL)
	select {
	case <-time.After(10 * time.Minute):
	case <-t.Context().Done():
	}
}

func TestVisualReviewHTTPRoundTripAndStaleRound(t *testing.T) {
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue := telemetry.Issue{ID: "issue-ui", Identifier: "digitaldrywood/detent#42", ProjectID: "detent", State: "Human Review", PullRequest: &telemetry.PullRequest{Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42", HeadSHA: head}}
	otherIssue := issue
	otherIssue.ID = "issue-other"
	if err := deps.Hub.Publish(telemetry.Snapshot{Project: telemetry.Project{ID: "detent"}, BoardIssues: []telemetry.Issue{issue, otherIssue}}); err != nil {
		t.Fatal(err)
	}
	mediaDir := t.TempDir()
	server, err := web.NewServer(web.Config{RuntimeDBPath: t.TempDir() + "/runtime.db", VisualReviewMediaDir: mediaDir, ServerAddress: "127.0.0.1:0", GlobalConfig: globalconfig.Config{APIToken: "review-token"}}, deps)
	if err != nil {
		t.Fatal(err)
	}

	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	manifest := visualReviewManifest(t, "round-1", head, png)
	unauthorized := visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipart(t, manifest, png), "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized import status=%d", unauthorized.Code)
	}
	var bad map[string]any
	if err := json.Unmarshal(manifest, &bad); err != nil {
		t.Fatal(err)
	}
	bad["assets"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	badManifest, _ := json.Marshal(bad)
	response := visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipart(t, badManifest, png), "review-token")
	if response.Code != http.StatusInternalServerError && response.Code != http.StatusBadRequest {
		t.Fatalf("bad hash status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-1", nil, "review-token")
	if response.Code != http.StatusNotFound {
		t.Fatalf("bad hash persisted capture: %d", response.Code)
	}
	orphanDir := filepath.Join(mediaDir, visualReviewTestKey("detent"), "round-1")
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, visualReviewTestKey("screenshot")+".png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipart(t, manifest, png), "review-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("recover matching orphan import status=%d body=%s", response.Code, response.Body.String())
	}
	page := visualReviewRequest(t, server.Handler(), http.MethodGet, "/projects/detent/visual-reviews/round-1/", nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) == 0 {
		t.Fatalf("visual review page status=%d cookies=%v", page.Code, page.Result().Cookies())
	}
	response = visualReviewUICookieRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-1", nil, page.Result().Cookies())
	if response.Code != http.StatusOK {
		t.Fatalf("UI-cookie review load status=%d body=%s", response.Code, response.Body.String())
	}
	conflictManifest := visualReviewManifest(t, "round-orphan-conflict", head, png)
	conflictDir := filepath.Join(mediaDir, visualReviewTestKey("detent"), "round-orphan-conflict")
	if err := os.MkdirAll(conflictDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conflictDir, visualReviewTestKey("screenshot")+".png"), []byte("not the expected media"), 0o600); err != nil {
		t.Fatal(err)
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipart(t, conflictManifest, png), "review-token")
	if response.Code != http.StatusConflict {
		t.Fatalf("mismatching orphan import status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/keys", strings.NewReader(`{"name":"Other project reader","scopes":["read"],"project_ids":["other-project"],"expires_in":"30d"}`), "review-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("create scoped read key status=%d body=%s", response.Code, response.Body.String())
	}
	var keyResponse struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &keyResponse); err != nil || keyResponse.Token == "" {
		t.Fatalf("decode scoped read key: %v body=%s", err, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-1", nil, keyResponse.Token)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-project capture read status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/visual-reviews/summary?project=detent&issue=issue-ui&head="+head, nil, keyResponse.Token)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-project summary read status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipartForIssue(t, "issue-other", manifest, png), "review-token")
	if response.Code != http.StatusConflict {
		t.Fatalf("cross-issue duplicate status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/board/card?project=detent&issue=issue-ui", nil, "review-token")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("visual-review-summary")) {
		t.Fatalf("Human Review sheet missing visual summary: %s", response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/visual-reviews/summary?project=detent&issue=issue-ui&head="+head, nil, "review-token")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("Review visual evidence")) {
		t.Fatalf("summary missing review link: %s", response.Body.String())
	}

	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-1", nil, "review-token")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"writable":true`)) || !bytes.Contains(response.Body.Bytes(), []byte(`media/screenshot.png`)) {
		t.Fatalf("review response status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-1/media/screenshot.png", nil, "review-token")
	if response.Code != http.StatusOK || response.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(response.Body.Bytes(), png) {
		t.Fatalf("media status=%d headers=%v", response.Code, response.Header())
	}

	feedback := map[string]any{"schema_version": 1, "repository": "digitaldrywood/detent", "pr": 42, "capture_id": "round-1", "head_sha": head, "authenticated": false, "author": "Reviewer", "exported_at": "2026-09-04T13:00:00Z", "recommendation": "draft", "asset_approvals": []any{}, "drafts": map[string]any{}, "annotations": []any{}}
	requestBody, _ := json.Marshal(map[string]any{"capture_id": "round-1", "head_sha": head, "expected_revision": 0, "feedback": feedback})
	response = visualReviewUICookieRequest(t, server.Handler(), http.MethodPut, "/api/v1/projects/detent/visual-reviews/round-1/draft", bytes.NewReader(requestBody), page.Result().Cookies())
	if response.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodPut, "/api/v1/projects/detent/visual-reviews/round-1/draft", bytes.NewReader(requestBody), "review-token")
	if response.Code != http.StatusConflict {
		t.Fatalf("stale revision status=%d body=%s", response.Code, response.Body.String())
	}

	manifest = visualReviewManifest(t, "round-2", head, png)
	response = visualReviewRequest(t, server.Handler(), http.MethodPost, "/api/v1/projects/detent/visual-reviews/import", visualReviewMultipart(t, manifest, png), "review-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("second import status=%d body=%s", response.Code, response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-1", nil, "review-token")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"writable":false`)) || !bytes.Contains(response.Body.Bytes(), []byte(`round-2`)) {
		t.Fatalf("historical response=%s", response.Body.String())
	}
	requestBody, _ = json.Marshal(map[string]any{"capture_id": "round-1", "head_sha": head, "expected_revision": 1, "feedback": feedback})
	response = visualReviewRequest(t, server.Handler(), http.MethodPut, "/api/v1/projects/detent/visual-reviews/round-1/draft", bytes.NewReader(requestBody), "review-token")
	if response.Code != http.StatusConflict {
		t.Fatalf("historical save status=%d", response.Code)
	}
	issue.PullRequest.HeadSHA = strings.Repeat("c", 40)
	if err := deps.Hub.Publish(telemetry.Snapshot{Project: telemetry.Project{ID: "detent"}, BoardIssues: []telemetry.Issue{issue, otherIssue}}); err != nil {
		t.Fatal(err)
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-2", nil, "review-token")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"writable":false`)) {
		t.Fatalf("changed-head response=%s", response.Body.String())
	}
	mediaPath := filepath.Join(mediaDir, visualReviewTestKey("detent"), "round-2", visualReviewTestKey("screenshot")+".png")
	data, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(mediaPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	issue.PullRequest.HeadSHA = head
	if err := deps.Hub.Publish(telemetry.Snapshot{Project: telemetry.Project{ID: "detent"}, BoardIssues: []telemetry.Issue{issue, otherIssue}}); err != nil {
		t.Fatal(err)
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-2", nil, "review-token")
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("missing or changed")) {
		t.Fatalf("same-size tamper not detected: %s", response.Body.String())
	}
	response = visualReviewRequest(t, server.Handler(), http.MethodGet, "/api/v1/projects/detent/visual-reviews/round-2/media/screenshot.png", nil, "review-token")
	if response.Code != http.StatusConflict {
		t.Fatalf("tampered media status=%d", response.Code)
	}
}

func visualReviewManifest(t *testing.T, captureID, head string, pngBytes []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(pngBytes)
	config, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"schema_version": 1, "capture_id": captureID, "repository": "digitaldrywood/detent", "pr": 42, "head_sha": head, "base_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "captured_at": "2026-09-04T12:00:00Z", "title": "UI review", "summary": "Button changed", "coverage_notes": "Test fixture", "changed_files": []any{"page.templ"},
		"assets":  []any{map[string]any{"id": "screenshot", "path": "media/screenshot.png", "kind": "after", "label": "Page", "observed": "Rendered", "inspected": true, "width": config.Width, "height": config.Height, "sha256": hex.EncodeToString(sum[:]), "source": map[string]any{"commit": head, "url": "http://127.0.0.1/page", "provenance": "isolated test fixture", "state": "default", "role": "operator", "theme": "light", "conditions": "fixed", "viewport": map[string]any{"width": 1280, "height": 800}}}},
		"changes": []any{map[string]any{"id": "button", "title": "Button", "description": "Changed", "files": []any{"page.templ"}, "status": "captured", "asset_ids": []any{"screenshot"}}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func visualReviewTestKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func visualReviewHarnessPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 960, 540))
	fill := func(rect image.Rectangle, value color.RGBA) {
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				canvas.SetRGBA(x, y, value)
			}
		}
	}
	fill(canvas.Bounds(), color.RGBA{R: 246, G: 248, B: 249, A: 255})
	fill(image.Rect(0, 0, 960, 72), color.RGBA{R: 18, G: 103, B: 93, A: 255})
	fill(image.Rect(72, 120, 888, 468), color.RGBA{R: 255, G: 255, B: 255, A: 255})
	fill(image.Rect(116, 176, 620, 212), color.RGBA{R: 223, G: 230, B: 233, A: 255})
	fill(image.Rect(116, 244, 420, 280), color.RGBA{R: 223, G: 230, B: 233, A: 255})
	fill(image.Rect(700, 388, 842, 432), color.RGBA{R: 18, G: 103, B: 93, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, canvas); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func visualReviewMultipart(t *testing.T, manifest, png []byte) io.Reader {
	return visualReviewMultipartForIssue(t, "issue-ui", manifest, png)
}

func visualReviewMultipartForIssue(t *testing.T, issueID string, manifest, png []byte) io.Reader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("issue_id", issueID); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("manifest", "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(manifest)
	part, err = writer.CreateFormFile("asset:screenshot", "screenshot.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(png)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &multipartReader{Reader: &body, contentType: writer.FormDataContentType()}
}

type multipartReader struct {
	io.Reader
	contentType string
}

func visualReviewRequest(t *testing.T, handler http.Handler, method, path string, body io.Reader, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), method, path, body)
	if typed, ok := body.(*multipartReader); ok {
		request.Header.Set("Content-Type", typed.contentType)
	} else if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func visualReviewUICookieRequest(t *testing.T, handler http.Handler, method, path string, body io.Reader, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), method, path, body)
	request.Header.Set("HX-Request", "true")
	request.Header.Set("Origin", "http://"+request.Host)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
