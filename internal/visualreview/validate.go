package visualreview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const MaxJSONBytes = 5 << 20
const maxItems, maxTextBytes = 2000, 20000

var (
	idPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern        = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
	digestPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	colorPattern      = regexp.MustCompile(`^#[A-Fa-f0-9]{6}$`)
)

type manifestJSON struct {
	SchemaVersion int          `json:"schema_version"`
	CaptureID     string       `json:"capture_id"`
	Repository    string       `json:"repository"`
	PR            *int         `json:"pr"`
	HeadSHA       string       `json:"head_sha"`
	BaseSHA       string       `json:"base_sha"`
	CapturedAt    string       `json:"captured_at"`
	Title         string       `json:"title"`
	Summary       string       `json:"summary"`
	CoverageNotes string       `json:"coverage_notes"`
	ChangedFiles  []string     `json:"changed_files"`
	Assets        []assetJSON  `json:"assets"`
	Changes       []changeJSON `json:"changes"`
}
type assetJSON struct {
	ID        string      `json:"id"`
	Path      string      `json:"path"`
	Kind      string      `json:"kind"`
	SHA256    string      `json:"sha256,omitempty"`
	Label     string      `json:"label"`
	Observed  string      `json:"observed"`
	Width     *int        `json:"width"`
	Height    *int        `json:"height"`
	Duration  *float64    `json:"duration,omitempty"`
	Inspected *bool       `json:"inspected"`
	Source    *sourceJSON `json:"source"`
	ParentID  string      `json:"parent_id,omitempty"`
	Crop      *cropJSON   `json:"crop,omitempty"`
}
type sourceJSON struct {
	Commit     string        `json:"commit"`
	URL        string        `json:"url"`
	Provenance string        `json:"provenance"`
	State      string        `json:"state"`
	Role       string        `json:"role"`
	Theme      string        `json:"theme"`
	Conditions string        `json:"conditions"`
	Viewport   *viewportJSON `json:"viewport"`
}
type viewportJSON struct {
	Width  *int `json:"width"`
	Height *int `json:"height"`
}
type cropJSON struct {
	X      *float64 `json:"x"`
	Y      *float64 `json:"y"`
	Width  *float64 `json:"width"`
	Height *float64 `json:"height"`
}
type changeJSON struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason,omitempty"`
	AssetIDs    []string `json:"asset_ids"`
}

func ValidateManifest(raw []byte) (Manifest, error) {
	if err := bodySize(raw, "manifest"); err != nil {
		return Manifest{}, err
	}
	var w manifestJSON
	if err := decodeStrict(raw, &w); err != nil {
		return Manifest{}, fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if err := rejectManifestNulls(raw); err != nil {
		return Manifest{}, err
	}
	if w.SchemaVersion != 1 {
		return Manifest{}, errors.New("unsupported manifest schema_version")
	}
	if !validID(w.CaptureID) || !repositoryPattern.MatchString(w.Repository) || w.PR == nil || *w.PR < 1 || int64(*w.PR) > 9007199254740991 || !validSHA(w.HeadSHA) || !validSHA(w.BaseSHA) {
		return Manifest{}, errors.New("invalid manifest identity")
	}
	if err := texts(map[string]string{"title": w.Title, "summary": w.Summary, "coverage_notes": w.CoverageNotes}); err != nil {
		return Manifest{}, err
	}
	at, err := time.Parse(time.RFC3339, w.CapturedAt)
	if err != nil {
		return Manifest{}, errors.New("invalid captured_at")
	}
	if len(w.ChangedFiles) == 0 || len(w.ChangedFiles) > maxItems || w.Assets == nil || len(w.Assets) > maxItems || len(w.Changes) == 0 || len(w.Changes) > maxItems {
		return Manifest{}, errors.New("inventory arrays are missing, empty, or too large")
	}
	if err := uniqueText(w.ChangedFiles, "changed_files"); err != nil {
		return Manifest{}, err
	}
	m := Manifest{SchemaVersion: 1, CaptureID: w.CaptureID, Repository: w.Repository, PR: *w.PR, HeadSHA: w.HeadSHA, BaseSHA: w.BaseSHA, CapturedAt: at, Title: w.Title, Summary: w.Summary, CoverageNotes: w.CoverageNotes, ChangedFiles: append([]string(nil), w.ChangedFiles...), Raw: append(json.RawMessage(nil), raw...)}
	assets, paths := map[string]ManifestAsset{}, map[string]struct{}{}
	for _, a := range w.Assets {
		v, e := validateAsset(a, w.HeadSHA)
		if e != nil {
			return Manifest{}, e
		}
		if _, ok := assets[v.ID]; ok {
			return Manifest{}, fmt.Errorf("duplicate asset ID %q", v.ID)
		}
		if _, ok := paths[v.Path]; ok {
			return Manifest{}, fmt.Errorf("duplicate asset path %q", v.Path)
		}
		assets[v.ID] = v
		paths[v.Path] = struct{}{}
		m.Assets = append(m.Assets, v)
	}
	for i, a := range m.Assets {
		wA := w.Assets[i]
		if wA.ParentID == "" {
			if wA.Crop != nil {
				return Manifest{}, fmt.Errorf("asset %q crop requires parent_id", a.ID)
			}
			continue
		}
		p, ok := assets[wA.ParentID]
		if !ok || p.ID == a.ID || p.Kind == "video" || p.ParentID != "" || p.Source.Commit != a.Source.Commit || wA.Crop == nil {
			return Manifest{}, fmt.Errorf("asset %q has invalid parent", a.ID)
		}
		c, e := validateCrop(*wA.Crop, p)
		if e != nil {
			return Manifest{}, fmt.Errorf("asset %q: %w", a.ID, e)
		}
		m.Assets[i].Crop = &c
		assets[a.ID] = m.Assets[i]
	}
	known, covered, coveredAssets, changes := stringSet(m.ChangedFiles), map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for _, c := range w.Changes {
		v, e := validateChange(c, known, assets, covered, coveredAssets)
		if e != nil {
			return Manifest{}, e
		}
		if _, ok := changes[v.ID]; ok {
			return Manifest{}, fmt.Errorf("duplicate change ID %q", v.ID)
		}
		changes[v.ID] = struct{}{}
		m.Changes = append(m.Changes, v)
	}
	for id := range assets {
		if _, ok := coveredAssets[id]; !ok {
			return Manifest{}, fmt.Errorf("asset %q is not assigned to a change", id)
		}
	}
	if len(covered) != len(known) {
		return Manifest{}, errors.New("changed file missing from inventory")
	}
	return m, nil
}

func validateAsset(a assetJSON, head string) (ManifestAsset, error) {
	if !validID(a.ID) || !safeMediaPath(a.Path) {
		return ManifestAsset{}, errors.New("invalid asset identity or path")
	}
	if err := texts(map[string]string{"asset label": a.Label, "observed": a.Observed}); err != nil {
		return ManifestAsset{}, err
	}
	if a.Width == nil || a.Height == nil || *a.Width < 1 || *a.Width > 50000 || *a.Height < 1 || *a.Height > 100000 || a.Inspected == nil || a.Source == nil {
		return ManifestAsset{}, fmt.Errorf("asset %q has missing or invalid fields", a.ID)
	}
	if !map[string]bool{"before": true, "after": true, "detail": true, "video": true}[a.Kind] || !validMediaExtension(a.Path, a.Kind) {
		return ManifestAsset{}, fmt.Errorf("asset %q has unsupported kind or extension", a.ID)
	}
	duration := 0.0
	if a.Kind == "video" {
		if a.Duration == nil || !finite(*a.Duration, .01, 14400) {
			return ManifestAsset{}, fmt.Errorf("asset %q has invalid duration", a.ID)
		}
		duration = *a.Duration
	}
	if a.SHA256 != "" && !digestPattern.MatchString(a.SHA256) {
		return ManifestAsset{}, fmt.Errorf("asset %q has invalid digest", a.ID)
	}
	s, e := validateSource(*a.Source)
	if e != nil {
		return ManifestAsset{}, fmt.Errorf("asset %q: %w", a.ID, e)
	}
	if a.Kind != "before" && s.Commit != head {
		return ManifestAsset{}, fmt.Errorf("asset %q does not match head SHA", a.ID)
	}
	return ManifestAsset{ID: a.ID, Path: a.Path, Kind: a.Kind, SHA256: a.SHA256, Label: a.Label, Observed: a.Observed, Width: *a.Width, Height: *a.Height, Duration: duration, Inspected: *a.Inspected, Source: s, ParentID: a.ParentID}, nil
}
func validateSource(s sourceJSON) (ManifestSource, error) {
	if !validSHA(s.Commit) || s.Viewport == nil || s.Viewport.Width == nil || s.Viewport.Height == nil || *s.Viewport.Width < 1 || *s.Viewport.Width > 10000 || *s.Viewport.Height < 1 || *s.Viewport.Height > 10000 {
		return ManifestSource{}, errors.New("invalid source commit or viewport")
	}
	if err := texts(map[string]string{"source.url": s.URL, "provenance": s.Provenance, "state": s.State, "role": s.Role, "theme": s.Theme, "conditions": s.Conditions}); err != nil {
		return ManifestSource{}, err
	}
	u, e := url.Parse(s.URL)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ManifestSource{}, errors.New("source URL must use HTTP(S)")
	}
	return ManifestSource{Commit: s.Commit, URL: s.URL, Provenance: s.Provenance, State: s.State, Role: s.Role, Theme: s.Theme, Conditions: s.Conditions, Viewport: Viewport{Width: *s.Viewport.Width, Height: *s.Viewport.Height}}, nil
}
func validateCrop(c cropJSON, p ManifestAsset) (Crop, error) {
	if c.X == nil || c.Y == nil || c.Width == nil || c.Height == nil || !finite(*c.X, 0, float64(p.Width)) || !finite(*c.Y, 0, float64(p.Height)) || !finite(*c.Width, 1, float64(p.Width)) || !finite(*c.Height, 1, float64(p.Height)) || *c.X+*c.Width > float64(p.Width) || *c.Y+*c.Height > float64(p.Height) {
		return Crop{}, errors.New("invalid crop bounds")
	}
	return Crop{X: *c.X, Y: *c.Y, Width: *c.Width, Height: *c.Height}, nil
}
func validateChange(c changeJSON, files map[string]struct{}, assets map[string]ManifestAsset, covered, coveredAssets map[string]struct{}) (ManifestChange, error) {
	if !validID(c.ID) || c.Files == nil || len(c.Files) == 0 || len(c.Files) > maxItems || c.AssetIDs == nil || len(c.AssetIDs) > maxItems {
		return ManifestChange{}, errors.New("invalid change identity or arrays")
	}
	if err := texts(map[string]string{"change title": c.Title, "change description": c.Description}); err != nil {
		return ManifestChange{}, err
	}
	if err := uniqueText(c.Files, "change files"); err != nil {
		return ManifestChange{}, err
	}
	if err := unique(c.AssetIDs, "change assets"); err != nil {
		return ManifestChange{}, err
	}
	for _, f := range c.Files {
		if _, ok := files[f]; !ok {
			return ManifestChange{}, fmt.Errorf("change %q references unknown file", c.ID)
		}
		covered[f] = struct{}{}
	}
	for _, id := range c.AssetIDs {
		if _, ok := assets[id]; !ok {
			return ManifestChange{}, fmt.Errorf("change %q references unknown asset", c.ID)
		}
		coveredAssets[id] = struct{}{}
	}
	switch c.Status {
	case "captured":
		hasAfter := false
		for _, id := range c.AssetIDs {
			a := assets[id]
			hasAfter = hasAfter || a.Kind == "after" || a.Kind == "detail"
			if !a.Inspected {
				return ManifestChange{}, fmt.Errorf("captured change %q has uninspected evidence", c.ID)
			}
		}
		if !hasAfter {
			return ManifestChange{}, fmt.Errorf("captured change %q requires after evidence", c.ID)
		}
	case "blocked", "not-ui":
		if err := text(c.Reason, "coverage reason"); err != nil {
			return ManifestChange{}, err
		}
	default:
		return ManifestChange{}, fmt.Errorf("change %q has invalid status", c.ID)
	}
	return ManifestChange{ID: c.ID, Title: c.Title, Description: c.Description, Files: append([]string(nil), c.Files...), Status: c.Status, Reason: c.Reason, AssetIDs: append([]string(nil), c.AssetIDs...)}, nil
}

type feedbackJSON struct {
	SchemaVersion  int               `json:"schema_version"`
	Repository     string            `json:"repository"`
	PR             *int              `json:"pr"`
	CaptureID      string            `json:"capture_id"`
	HeadSHA        string            `json:"head_sha"`
	Authenticated  *bool             `json:"authenticated"`
	Author         string            `json:"author"`
	Recommendation string            `json:"recommendation"`
	ExportedAt     string            `json:"exported_at"`
	Annotations    []annotationJSON  `json:"annotations"`
	AssetApprovals []approvalJSON    `json:"asset_approvals,omitempty"`
	Drafts         map[string]string `json:"drafts,omitempty"`
}
type approvalJSON struct {
	AssetID    string `json:"asset_id"`
	Author     string `json:"author"`
	ApprovedAt string `json:"approved_at"`
}
type annotationJSON struct {
	ID          string      `json:"id"`
	ChangeID    string      `json:"change_id"`
	AssetID     string      `json:"asset_id"`
	Kind        string      `json:"kind"`
	Text        string      `json:"text"`
	Author      string      `json:"author,omitempty"`
	CreatedAt   string      `json:"created_at"`
	X           *float64    `json:"x,omitempty"`
	Y           *float64    `json:"y,omitempty"`
	Width       *float64    `json:"width,omitempty"`
	Height      *float64    `json:"height,omitempty"`
	EndX        *float64    `json:"end_x,omitempty"`
	EndY        *float64    `json:"end_y,omitempty"`
	Time        *float64    `json:"time,omitempty"`
	Points      []pointJSON `json:"points,omitempty"`
	Color       string      `json:"color,omitempty"`
	StrokeWidth *float64    `json:"stroke_width,omitempty"`
}
type pointJSON struct {
	X *float64 `json:"x"`
	Y *float64 `json:"y"`
}

func ValidateFeedback(raw []byte, capture Capture) error {
	if err := bodySize(raw, "feedback"); err != nil {
		return err
	}
	var f feedbackJSON
	if err := decodeStrict(raw, &f); err != nil {
		return fmt.Errorf("invalid feedback JSON: %w", err)
	}
	if err := rejectFeedbackNulls(raw); err != nil {
		return err
	}
	m, err := manifestForCapture(capture)
	if err != nil {
		return err
	}
	if f.SchemaVersion != 1 || f.PR == nil || f.Repository != m.Repository || *f.PR != m.PR || f.CaptureID != m.CaptureID || f.HeadSHA != m.HeadSHA {
		return fmt.Errorf("%w: feedback does not belong to capture", ErrConflict)
	}
	if f.Authenticated == nil || *f.Authenticated {
		return errors.New("local feedback must be unauthenticated")
	}
	if err := text(f.Author, "author"); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339, f.ExportedAt); err != nil {
		return errors.New("invalid exported_at")
	}
	if !map[string]bool{"draft": true, "request-changes": true, "recommend-approval": true}[f.Recommendation] {
		return errors.New("invalid recommendation")
	}
	if f.Annotations == nil || len(f.Annotations) > maxItems || len(f.AssetApprovals) > maxItems || len(f.Drafts) > maxItems {
		return errors.New("feedback arrays or drafts are missing or too large")
	}
	assets := map[string]ManifestAsset{}
	for _, a := range m.Assets {
		assets[a.ID] = a
	}
	changes := map[string]ManifestChange{}
	blocked := false
	for _, c := range m.Changes {
		changes[c.ID] = c
		blocked = blocked || c.Status == "blocked"
	}
	approved := map[string]struct{}{}
	for _, a := range f.AssetApprovals {
		if _, ok := assets[a.AssetID]; !ok {
			return errors.New("unknown approved asset")
		}
		if _, ok := approved[a.AssetID]; ok {
			return errors.New("duplicate asset approval")
		}
		approved[a.AssetID] = struct{}{}
		if err := text(a.Author, "approval author"); err != nil {
			return err
		}
		if _, err := time.Parse(time.RFC3339, a.ApprovedAt); err != nil {
			return errors.New("invalid approved_at")
		}
	}
	if f.Recommendation == "recommend-approval" && (blocked || len(approved) != len(assets)) {
		return errors.New("incomplete review cannot recommend approval")
	}
	seen := map[string]struct{}{}
	for _, a := range f.Annotations {
		if err := validateAnnotation(a, assets, changes); err != nil {
			return err
		}
		if _, ok := seen[a.ID]; ok {
			return errors.New("duplicate annotation ID")
		}
		seen[a.ID] = struct{}{}
	}
	for id, draft := range f.Drafts {
		if _, ok := changes[id]; !ok || len(draft) > maxTextBytes {
			return errors.New("invalid unsent draft")
		}
	}
	return nil
}
func validateAnnotation(a annotationJSON, assets map[string]ManifestAsset, changes map[string]ManifestChange) error {
	if !validID(a.ID) {
		return errors.New("invalid annotation ID")
	}
	if err := text(a.Text, "annotation text"); err != nil {
		return err
	}
	if a.Author != "" {
		if err := text(a.Author, "annotation author"); err != nil {
			return err
		}
	}
	if _, err := time.Parse(time.RFC3339, a.CreatedAt); err != nil {
		return errors.New("invalid annotation date")
	}
	c, ok := changes[a.ChangeID]
	if !ok {
		return errors.New("unknown change in feedback")
	}
	asset, ok := assets[a.AssetID]
	if !ok || !contains(c.AssetIDs, a.AssetID) {
		return errors.New("unknown asset in feedback")
	}
	if !map[string]bool{"note": true, "pin": true, "rectangle": true, "ellipse": true, "arrow": true, "pen": true, "text": true, "timestamp": true}[a.Kind] {
		return errors.New("invalid annotation kind")
	}
	if a.Color != "" && !colorPattern.MatchString(a.Color) {
		return errors.New("invalid annotation color")
	}
	if a.StrokeWidth != nil && !finite(*a.StrokeWidth, 1, 12) {
		return errors.New("invalid stroke width")
	}
	if a.Kind == "timestamp" {
		if asset.Kind != "video" || a.Time == nil || !finite(*a.Time, 0, asset.Duration) {
			return errors.New("invalid video timestamp")
		}
		return nil
	}
	if !map[string]bool{"pin": true, "rectangle": true, "ellipse": true, "arrow": true, "pen": true, "text": true}[a.Kind] {
		return nil
	}
	if asset.Kind == "video" || a.X == nil || a.Y == nil || !finite(*a.X, 0, 1) || !finite(*a.Y, 0, 1) {
		return errors.New("invalid annotation coordinates")
	}
	if a.Kind == "rectangle" || a.Kind == "ellipse" {
		if a.Width == nil || a.Height == nil || !finite(*a.Width, .001, 1) || !finite(*a.Height, .001, 1) || *a.X+*a.Width > 1.000001 || *a.Y+*a.Height > 1.000001 {
			return errors.New("annotation outside image")
		}
	}
	if a.Kind == "arrow" && (a.EndX == nil || a.EndY == nil || !finite(*a.EndX, 0, 1) || !finite(*a.EndY, 0, 1)) {
		return errors.New("invalid arrow coordinates")
	}
	if a.Kind == "pen" {
		if len(a.Points) < 2 || len(a.Points) > maxItems {
			return errors.New("invalid pen points")
		}
		for _, p := range a.Points {
			if p.X == nil || p.Y == nil || !finite(*p.X, 0, 1) || !finite(*p.Y, 0, 1) {
				return errors.New("invalid pen point")
			}
		}
	}
	return nil
}

func manifestForCapture(c Capture) (Manifest, error) {
	if len(c.ManifestJSON) == 0 {
		return Manifest{}, errors.New("capture manifest is required for feedback validation")
	}
	m, err := ValidateManifest(c.ManifestJSON)
	if err != nil {
		return Manifest{}, fmt.Errorf("stored capture manifest is invalid: %w", err)
	}
	if m.Repository != c.Repository || m.PR != c.PR || m.CaptureID != c.CaptureID || m.HeadSHA != c.HeadSHA {
		return Manifest{}, fmt.Errorf("%w: stored manifest does not match capture", ErrConflict)
	}
	return m, nil
}
func decodeStrict(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err != nil {
			return fmt.Errorf("invalid trailing JSON: %w", err)
		}
		return errors.New("multiple JSON values")
	}
	return nil
}
func bodySize(raw []byte, name string) error {
	if len(raw) == 0 || len(raw) > MaxJSONBytes {
		return fmt.Errorf("%s body size invalid", name)
	}
	return nil
}
func text(v, name string) error {
	if strings.TrimSpace(v) == "" || len(v) > maxTextBytes {
		return fmt.Errorf("%s: expected nonempty bounded text", name)
	}
	return nil
}
func texts(m map[string]string) error {
	for n, v := range m {
		if err := text(v, n); err != nil {
			return err
		}
	}
	return nil
}
func validID(v string) bool  { return idPattern.MatchString(v) }
func validSHA(v string) bool { return shaPattern.MatchString(v) }
func finite(v, min, max float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= min && v <= max
}
func unique(v []string, name string) error {
	s := map[string]struct{}{}
	for _, x := range v {
		if _, ok := s[x]; ok {
			return fmt.Errorf("%s contains duplicate values", name)
		}
		s[x] = struct{}{}
	}
	return nil
}
func uniqueText(v []string, name string) error {
	if err := unique(v, name); err != nil {
		return err
	}
	for _, x := range v {
		if err := text(x, name); err != nil {
			return err
		}
	}
	return nil
}
func stringSet(v []string) map[string]struct{} {
	m := map[string]struct{}{}
	for _, x := range v {
		m[x] = struct{}{}
	}
	return m
}
func contains(v []string, w string) bool {
	for _, x := range v {
		if x == w {
			return true
		}
	}
	return false
}
func safeMediaPath(v string) bool {
	if len(v) == 0 || len(v) > maxTextBytes || strings.HasPrefix(v, "/") || path.Clean(v) != v || !strings.HasPrefix(v, "media/") {
		return false
	}
	for _, r := range v {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("_./-", r) {
			return false
		}
	}
	return true
}
func validMediaExtension(v, kind string) bool {
	e := strings.ToLower(path.Ext(v))
	if kind == "video" {
		return e == ".mp4" || e == ".webm"
	}
	return e == ".png" || e == ".jpg" || e == ".jpeg" || e == ".webp"
}

func rejectManifestNulls(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	var assets []map[string]json.RawMessage
	if err := json.Unmarshal(root["assets"], &assets); err != nil {
		return err
	}
	for _, asset := range assets {
		if isNull(asset["sha256"]) {
			return errors.New("invalid media digest")
		}
	}
	return nil
}

func rejectFeedbackNulls(raw []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return err
	}
	if isNull(root["drafts"]) {
		return errors.New("invalid draft map")
	}
	if draftsRaw := root["drafts"]; draftsRaw != nil {
		var drafts map[string]json.RawMessage
		if err := json.Unmarshal(draftsRaw, &drafts); err != nil {
			return errors.New("invalid draft map")
		}
		for _, draft := range drafts {
			if isNull(draft) {
				return errors.New("invalid unsent draft")
			}
		}
	}
	var annotations []map[string]json.RawMessage
	if err := json.Unmarshal(root["annotations"], &annotations); err != nil {
		return err
	}
	for _, annotation := range annotations {
		for _, field := range []string{"author", "color", "stroke_width"} {
			if isNull(annotation[field]) {
				return fmt.Errorf("annotation %s cannot be null", field)
			}
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return raw != nil && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
