package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	_ "golang.org/x/image/webp"

	"github.com/digitaldrywood/detent/internal/apikey"
	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/visualreview"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	visualReviewMaxRequestBytes = 512 << 20
	visualReviewMaxAssetBytes   = 250 << 20
)

func closeVisualReviewReader(reader io.Closer) {
	if err := reader.Close(); err != nil {
		return
	}
}

func cleanupVisualReviewMultipart(form *multipart.Form) {
	if err := form.RemoveAll(); err != nil {
		return
	}
}

func cleanupVisualReviewPath(path string) {
	if err := os.RemoveAll(path); err != nil {
		return
	}
}

type visualReviewResponse struct {
	Manifest       json.RawMessage     `json:"manifest"`
	Feedback       json.RawMessage     `json:"feedback,omitempty"`
	Revision       int64               `json:"revision"`
	Writable       bool                `json:"writable"`
	ReadOnlyReason string              `json:"read_only_reason,omitempty"`
	AuditActor     string              `json:"audit_actor,omitempty"`
	UpdatedAt      time.Time           `json:"updated_at,omitempty"`
	Rounds         []visualReviewRound `json:"rounds,omitempty"`
}

type visualReviewRound struct {
	CaptureID  string    `json:"capture_id"`
	HeadSHA    string    `json:"head_sha"`
	CapturedAt time.Time `json:"captured_at"`
	URL        string    `json:"url"`
}

type saveVisualReviewRequest struct {
	CaptureID        string          `json:"capture_id"`
	HeadSHA          string          `json:"head_sha"`
	ExpectedRevision int64           `json:"expected_revision"`
	Feedback         json.RawMessage `json:"feedback"`
}

func reviewMediaDir(dbPath, configured string) string {
	if dir := strings.TrimSpace(configured); dir != "" {
		return filepath.Clean(dir)
	}
	if dbPath = strings.TrimSpace(dbPath); dbPath != "" {
		return filepath.Clean(dbPath + ".visual-review")
	}
	return ""
}

func (s *Server) visualReviewPage(c echo.Context) error {
	if !strings.HasSuffix(c.Request().URL.Path, "/") {
		return c.Redirect(http.StatusPermanentRedirect, c.Request().URL.Path+"/")
	}
	data, err := fs.ReadFile(s.assets.fsys, "visual-review/host.html")
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "Visual review unavailable")
	}
	return c.Blob(http.StatusOK, "text/html; charset=utf-8", data)
}

func (s *Server) apiVisualReviewSummary(c echo.Context) error {
	projectID := strings.TrimSpace(c.QueryParam("project"))
	issueID := strings.TrimSpace(c.QueryParam("issue"))
	expectedHead := strings.TrimSpace(c.QueryParam("head"))
	if projectID == "" || issueID == "" || expectedHead == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "project, issue, and head are required")
	}
	if err := visualReviewProjectAllowed(c, projectID); err != nil {
		return err
	}
	capture, err := s.store.LatestVisualReviewCapture(c.Request().Context(), projectID, issueID)
	if errors.Is(err, visualreview.ErrNotFound) {
		return render(c, templates.VisualReviewSummary("", "", "", false, ""))
	}
	if err != nil {
		return err
	}
	currentHead, ok := s.currentVisualReviewHead(projectID, issueID)
	current := ok && currentHead == expectedHead && capture.HeadSHA == currentHead
	detail := "Evidence is retained for an earlier capture or PR head; it is read-only."
	if current {
		detail = "Annotations and per-asset recommendations persist on this Detent node."
	}
	path := fmt.Sprintf("/projects/%s/visual-reviews/%s/", url.PathEscape(projectID), url.PathEscape(capture.CaptureID))
	return render(c, templates.VisualReviewSummary(capture.CaptureID, capture.Title, path, current, detail))
}

func (s *Server) apiVisualReview(c echo.Context) error {
	projectID, captureID := strings.TrimSpace(c.Param("project_id")), strings.TrimSpace(c.Param("capture_id"))
	if err := visualReviewProjectAllowed(c, projectID); err != nil {
		return err
	}
	capture, err := s.store.VisualReviewCapture(c.Request().Context(), projectID, captureID)
	if errors.Is(err, visualreview.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "Visual review not found")
	}
	if err != nil {
		return err
	}
	draft, err := s.store.VisualReviewDraft(c.Request().Context(), projectID, captureID)
	if err != nil {
		return err
	}
	writable, reason := s.visualReviewWritable(c.Request().Context(), capture)
	rounds, err := s.store.ListVisualReviewCaptures(c.Request().Context(), projectID, capture.IssueID)
	if err != nil {
		return err
	}
	roundLinks := make([]visualReviewRound, 0, len(rounds))
	for _, round := range rounds {
		roundLinks = append(roundLinks, visualReviewRound{CaptureID: round.CaptureID, HeadSHA: round.HeadSHA, CapturedAt: round.CapturedAt, URL: fmt.Sprintf("/projects/%s/visual-reviews/%s/", url.PathEscape(projectID), url.PathEscape(round.CaptureID))})
	}
	manifest, err := hostedVisualReviewManifest(capture.ManifestJSON, capture.Assets)
	if err != nil {
		return fmt.Errorf("prepare hosted visual review: %w", err)
	}
	return c.JSON(http.StatusOK, visualReviewResponse{Manifest: manifest, Feedback: draft.FeedbackJSON, Revision: draft.Revision, Writable: writable, ReadOnlyReason: reason, AuditActor: draft.AuditActor, UpdatedAt: draft.UpdatedAt, Rounds: roundLinks})
}

func hostedVisualReviewManifest(raw json.RawMessage, assets []visualreview.Asset) (json.RawMessage, error) {
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	items, ok := manifest["assets"].([]any)
	if !ok || len(items) != len(assets) {
		return nil, errors.New("manifest assets do not match persisted assets")
	}
	byID := make(map[string]visualreview.Asset, len(assets))
	for _, asset := range assets {
		byID[asset.ID] = asset
	}
	for _, item := range items {
		asset, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("invalid manifest asset")
		}
		id, ok := asset["id"].(string)
		if !ok {
			return nil, errors.New("manifest asset id is invalid")
		}
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("manifest asset %q is not persisted", id)
		}
		asset["path"] = "media/" + id + strings.ToLower(filepath.Ext(byID[id].StorageKey))
	}
	return json.Marshal(manifest)
}

func (s *Server) apiSaveVisualReviewDraft(c echo.Context) error {
	if !visualReviewMutationSourceAllowed(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "same-origin dashboard request or API token required")
	}
	projectID, captureID := strings.TrimSpace(c.Param("project_id")), strings.TrimSpace(c.Param("capture_id"))
	var request saveVisualReviewRequest
	if err := json.NewDecoder(io.LimitReader(c.Request().Body, 6<<20)).Decode(&request); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid visual review draft")
	}
	if request.CaptureID != captureID || request.ExpectedRevision < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "draft identity or revision is invalid")
	}
	capture, err := s.store.VisualReviewCapture(c.Request().Context(), projectID, captureID)
	if err != nil {
		return visualReviewHTTPError(err)
	}
	if writable, _ := s.visualReviewWritable(c.Request().Context(), capture); !writable || request.HeadSHA != capture.HeadSHA {
		return echo.NewHTTPError(http.StatusConflict, "visual review is stale or current PR head is unavailable")
	}
	if err := visualreview.ValidateFeedback(request.Feedback, capture); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	draft, err := s.store.SaveVisualReviewDraft(c.Request().Context(), projectID, captureID, request.HeadSHA, request.ExpectedRevision, request.Feedback, visualReviewAuditActor(c), s.now())
	if err != nil {
		return visualReviewHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"revision": draft.Revision, "updated_at": draft.UpdatedAt})
}

func visualReviewMutationSourceAllowed(request *http.Request) bool {
	return len(requestAPITokens(request)) > 0 || requestSameOriginDashboardSource(request)
}

func visualReviewAuditActor(c echo.Context) string {
	if session, ok := webSessionFromContext(c.Request().Context()); ok && strings.TrimSpace(session.Email) != "" {
		return "session:" + strings.TrimSpace(session.Email)
	}
	if credential, ok := apiCredentialFromContext(c.Request().Context()); ok && strings.TrimSpace(credential.ID) != "" {
		return "api:" + strings.TrimSpace(credential.ID)
	}
	return "local-operator"
}

func (s *Server) visualReviewWritable(ctx context.Context, capture visualreview.Capture) (bool, string) {
	if s.demo != nil {
		return false, "Demo visual reviews are read-only."
	}
	repository, pr, head, ok := s.currentVisualReviewIdentity(capture.ProjectID, capture.IssueID)
	if !ok {
		return false, "Current pull request head is unavailable. This capture is read-only."
	}
	if head != capture.HeadSHA || pr != capture.PR || !strings.EqualFold(repository, capture.Repository) {
		return false, "This capture belongs to an earlier pull request head and is read-only."
	}
	latest, err := s.store.LatestVisualReviewCapture(ctx, capture.ProjectID, capture.IssueID)
	if err != nil || latest.CaptureID != capture.CaptureID {
		return false, "A newer visual review capture exists. This historical round is read-only."
	}
	for _, asset := range capture.Assets {
		path, safe := visualReviewStoragePath(s.reviewMediaDir, asset.StorageKey)
		info, statErr := os.Stat(path)
		if !safe || statErr != nil || !info.Mode().IsRegular() || info.Size() != asset.SizeBytes {
			return false, "One or more evidence files are missing or changed. This capture is read-only."
		}
		digest, hashErr := visualReviewFileSHA256(path)
		if hashErr != nil || !strings.EqualFold(digest, asset.SHA256) {
			return false, "One or more evidence files are missing or changed. This capture is read-only."
		}
	}
	return true, ""
}

func (s *Server) currentVisualReviewHead(projectID, issueID string) (string, bool) {
	_, _, head, ok := s.currentVisualReviewIdentity(projectID, issueID)
	return head, ok
}

func (s *Server) currentVisualReviewIdentity(projectID, issueID string) (string, int, string, bool) {
	if s.hub == nil {
		return "", 0, "", false
	}
	snapshot, ok := s.hub.Latest()
	if !ok {
		return "", 0, "", false
	}
	for _, issue := range kanbanstate.SnapshotIssues(snapshot) {
		if !kanbanstate.SameIssue(issue, projectID, issueID, snapshot.Project.ID) || issue.PullRequest == nil {
			continue
		}
		head := strings.TrimSpace(issue.PullRequest.HeadSHA)
		repository := kanbanPullRequestRepository(issue)
		return repository, issue.PullRequest.Number, head, repository != "" && issue.PullRequest.Number > 0 && head != ""
	}
	return "", 0, "", false
}

func (s *Server) visualReviewMedia(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	if err := visualReviewProjectAllowed(c, projectID); err != nil {
		return err
	}
	capture, err := s.store.VisualReviewCapture(c.Request().Context(), projectID, strings.TrimSpace(c.Param("capture_id")))
	if err != nil {
		return visualReviewHTTPError(err)
	}
	var found visualreview.Asset
	for _, asset := range capture.Assets {
		servedID := asset.ID + strings.ToLower(filepath.Ext(asset.StorageKey))
		if servedID == c.Param("asset_id") {
			found = asset
			break
		}
	}
	if found.ID == "" || s.reviewMediaDir == "" {
		return echo.NewHTTPError(http.StatusNotFound, "Visual review media not found")
	}
	path, ok := visualReviewStoragePath(s.reviewMediaDir, found.StorageKey)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "Visual review media not found")
	}
	info, statErr := os.Stat(path)
	digest, hashErr := visualReviewFileSHA256(path)
	if statErr != nil || !info.Mode().IsRegular() || info.Size() != found.SizeBytes || hashErr != nil || !strings.EqualFold(digest, found.SHA256) {
		return echo.NewHTTPError(http.StatusConflict, "Visual review media is missing or changed")
	}
	c.Response().Header().Set("Content-Type", found.MediaType)
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.File(path)
}

func visualReviewProjectAllowed(c echo.Context, projectID string) error {
	credential, ok := apiCredentialFromContext(c.Request().Context())
	if ok && !apikey.AllowsProject(credential.ProjectIDs, projectID) {
		return echo.NewHTTPError(http.StatusForbidden, "API key is not allowed for project "+projectID)
	}
	return nil
}

func visualReviewStoragePath(root, key string) (string, bool) {
	root = filepath.Clean(root)
	path := filepath.Join(root, filepath.FromSlash(key))
	rel, err := filepath.Rel(root, path)
	return path, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func visualReviewHTTPError(err error) error {
	switch {
	case errors.Is(err, visualreview.ErrNotFound):
		return echo.NewHTTPError(http.StatusNotFound, "Visual review not found")
	case errors.Is(err, visualreview.ErrConflict):
		return echo.NewHTTPError(http.StatusConflict, "Visual review changed; reload before saving")
	default:
		return err
	}
}

func (s *Server) apiImportVisualReview(c echo.Context) error {
	if !visualReviewMutationSourceAllowed(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "same-origin dashboard request or API token required")
	}
	if s.demo != nil || s.reviewMediaDir == "" {
		return echo.NewHTTPError(http.StatusConflict, "visual review imports are unavailable in this runtime")
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, visualReviewMaxRequestBytes)
	if err := c.Request().ParseMultipartForm(8 << 20); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid or oversized visual review import")
	}
	if c.Request().MultipartForm != nil {
		defer cleanupVisualReviewMultipart(c.Request().MultipartForm)
	}
	manifestFile, _, err := c.Request().FormFile("manifest")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "manifest part is required")
	}
	defer closeVisualReviewReader(manifestFile)
	raw, err := io.ReadAll(io.LimitReader(manifestFile, 6<<20))
	if err != nil || len(raw) == 6<<20 {
		return echo.NewHTTPError(http.StatusBadRequest, "manifest is unreadable or oversized")
	}
	manifest, err := visualreview.ValidateManifest(raw)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	projectID, issueID := strings.TrimSpace(c.Param("project_id")), strings.TrimSpace(c.FormValue("issue_id"))
	if issueID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "issue_id is required")
	}
	repository, pr, head, ok := s.currentVisualReviewIdentity(projectID, issueID)
	if !ok || head != manifest.HeadSHA || pr != manifest.PR || !strings.EqualFold(repository, manifest.Repository) {
		return echo.NewHTTPError(http.StatusConflict, "manifest head is not the current pull request head")
	}
	capture, err := s.persistVisualReviewImport(c.Request().Context(), projectID, issueID, manifest, c.Request().MultipartForm)
	if err != nil {
		return visualReviewHTTPError(err)
	}
	return c.JSON(http.StatusCreated, map[string]any{"capture_id": capture.CaptureID, "head_sha": capture.HeadSHA, "review_url": fmt.Sprintf("/projects/%s/visual-reviews/%s/", url.PathEscape(projectID), url.PathEscape(capture.CaptureID))})
}

func (s *Server) persistVisualReviewImport(ctx context.Context, projectID, issueID string, manifest visualreview.Manifest, form *multipart.Form) (visualreview.Capture, error) {
	if existing, err := s.store.VisualReviewCapture(ctx, projectID, manifest.CaptureID); err == nil {
		if existing.IssueID == issueID && existing.Repository == manifest.Repository && existing.PR == manifest.PR && existing.HeadSHA == manifest.HeadSHA && string(existing.ManifestJSON) == string(manifest.Raw) && visualReviewCaptureMediaIntact(s.reviewMediaDir, existing) {
			return existing, nil
		}
		return visualreview.Capture{}, visualreview.ErrConflict
	} else if !errors.Is(err, visualreview.ErrNotFound) {
		return visualreview.Capture{}, err
	}
	stage, err := os.MkdirTemp(s.reviewMediaDir, ".import-")
	if err != nil {
		if err := os.MkdirAll(s.reviewMediaDir, 0o700); err != nil {
			return visualreview.Capture{}, err
		}
		stage, err = os.MkdirTemp(s.reviewMediaDir, ".import-")
	}
	if err != nil {
		return visualreview.Capture{}, err
	}
	defer cleanupVisualReviewPath(stage)
	assets := make([]visualreview.Asset, 0, len(manifest.Assets))
	for _, expected := range manifest.Assets {
		headers := form.File["asset:"+expected.ID]
		if len(headers) != 1 {
			return visualreview.Capture{}, fmt.Errorf("asset %q must have exactly one media part", expected.ID)
		}
		asset, err := stageVisualReviewAsset(stage, projectID, manifest.CaptureID, expected, headers[0])
		if err != nil {
			return visualreview.Capture{}, err
		}
		assets = append(assets, asset)
	}
	finalKey := filepath.ToSlash(filepath.Join(visualReviewKey(projectID), manifest.CaptureID))
	finalDir, ok := visualReviewStoragePath(s.reviewMediaDir, finalKey)
	if !ok {
		return visualreview.Capture{}, errors.New("invalid generated visual review storage path")
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o700); err != nil {
		return visualreview.Capture{}, err
	}
	if _, err := os.Stat(finalDir); err == nil {
		return visualreview.Capture{}, visualreview.ErrConflict
	} else if !errors.Is(err, fs.ErrNotExist) {
		return visualreview.Capture{}, err
	}
	if err := os.Rename(stage, finalDir); err != nil {
		return visualreview.Capture{}, fmt.Errorf("publish visual review media: %w", err)
	}
	for i := range assets {
		assets[i].StorageKey = filepath.ToSlash(filepath.Join(finalKey, filepath.Base(assets[i].StorageKey)))
	}
	capture := visualreview.Capture{ProjectID: projectID, IssueID: issueID, Repository: manifest.Repository, PR: manifest.PR, CaptureID: manifest.CaptureID, HeadSHA: manifest.HeadSHA, BaseSHA: manifest.BaseSHA, CapturedAt: manifest.CapturedAt, Title: manifest.Title, Summary: manifest.Summary, CoverageNotes: manifest.CoverageNotes, ManifestJSON: manifest.Raw, Assets: assets, CreatedAt: s.now()}
	if err := s.store.CreateVisualReviewCapture(ctx, capture); err != nil {
		cleanupVisualReviewPath(finalDir)
		return visualreview.Capture{}, err
	}
	return capture, nil
}

func stageVisualReviewAsset(stage, projectID, captureID string, expected visualreview.ManifestAsset, header *multipart.FileHeader) (visualreview.Asset, error) {
	source, err := header.Open()
	if err != nil {
		return visualreview.Asset{}, err
	}
	defer closeVisualReviewReader(source)
	ext := strings.ToLower(filepath.Ext(expected.Path))
	name := visualReviewKey(expected.ID) + ext
	target, err := os.OpenFile(filepath.Join(stage, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return visualreview.Asset{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(target, hash), io.LimitReader(source, visualReviewMaxAssetBytes+1))
	closeErr := target.Close()
	if copyErr != nil {
		return visualreview.Asset{}, copyErr
	}
	if closeErr != nil {
		return visualreview.Asset{}, closeErr
	}
	if size == 0 || size > visualReviewMaxAssetBytes {
		return visualreview.Asset{}, fmt.Errorf("asset %q is empty or exceeds 250 MB", expected.ID)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(digest, expected.SHA256) {
		return visualreview.Asset{}, fmt.Errorf("asset %q SHA-256 mismatch", expected.ID)
	}
	path := filepath.Join(stage, name)
	mediaType, width, height, err := inspectVisualReviewMedia(path, expected.Kind)
	if err != nil {
		return visualreview.Asset{}, fmt.Errorf("asset %q: %w", expected.ID, err)
	}
	if width > 0 && (width != expected.Width || height != expected.Height) {
		return visualreview.Asset{}, fmt.Errorf("asset %q dimensions mismatch", expected.ID)
	}
	return visualreview.Asset{ID: expected.ID, StorageKey: name, Kind: expected.Kind, MediaType: mediaType, SizeBytes: size, SHA256: digest, Width: expected.Width, Height: expected.Height}, nil
}

func inspectVisualReviewMedia(path, kind string) (string, int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer closeVisualReviewReader(file)
	buf := make([]byte, 512)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", 0, 0, err
	}
	buf = buf[:n]
	mediaType := http.DetectContentType(buf)
	if kind == "video" {
		mp4 := len(buf) >= 12 && string(buf[4:8]) == "ftyp"
		webm := len(buf) >= 4 && buf[0] == 0x1a && buf[1] == 0x45 && buf[2] == 0xdf && buf[3] == 0xa3
		if !mp4 && !webm {
			return "", 0, 0, errors.New("unsupported video signature")
		}
		if mp4 {
			return "video/mp4", 0, 0, nil
		}
		return "video/webm", 0, 0, nil
	}
	if mediaType != "image/png" && mediaType != "image/jpeg" && mediaType != "image/webp" {
		return "", 0, 0, errors.New("unsupported image signature")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, 0, err
	}
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return "", 0, 0, fmt.Errorf("decode image dimensions: %w", err)
	}
	return mediaType, config.Width, config.Height, nil
}

func visualReviewKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func visualReviewFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer closeVisualReviewReader(file)
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func visualReviewCaptureMediaIntact(root string, capture visualreview.Capture) bool {
	for _, asset := range capture.Assets {
		path, safe := visualReviewStoragePath(root, asset.StorageKey)
		if !safe {
			return false
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != asset.SizeBytes {
			return false
		}
		digest, err := visualReviewFileSHA256(path)
		if err != nil || !strings.EqualFold(digest, asset.SHA256) {
			return false
		}
	}
	return true
}
