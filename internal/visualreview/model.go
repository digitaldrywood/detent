package visualreview

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrConflict = errors.New("visual review conflict")
	ErrNotFound = errors.New("visual review not found")
)

type Asset struct {
	ID, StorageKey, Kind, MediaType, SHA256 string
	SourcePath                              string
	SizeBytes                               int64
	Width, Height                           int
}

type Capture struct {
	ProjectID, IssueID, Repository, CaptureID, HeadSHA, BaseSHA string
	PR                                                          int
	CapturedAt                                                  time.Time
	Title, Summary, CoverageNotes                               string
	ManifestJSON                                                json.RawMessage
	Assets                                                      []Asset
	CreatedAt                                                   time.Time
}

type Draft struct {
	ProjectID, CaptureID, HeadSHA string
	Revision                      int64
	FeedbackJSON                  json.RawMessage
	AuditActor                    string
	UpdatedAt                     time.Time
}

// Manifest is the validated v1 review-package manifest. Raw preserves the
// exact payload that passed validation for persistence and feedback binding.
type Manifest struct {
	SchemaVersion                 int
	CaptureID, Repository         string
	HeadSHA, BaseSHA              string
	PR                            int
	CapturedAt                    time.Time
	Title, Summary, CoverageNotes string
	ChangedFiles                  []string
	Assets                        []ManifestAsset
	Changes                       []ManifestChange
	Raw                           json.RawMessage
}

type ManifestAsset struct {
	ID, Path, Kind, SHA256 string
	Label, Observed        string
	Width, Height          int
	Duration               float64
	Inspected              bool
	Source                 ManifestSource
	ParentID               string
	Crop                   *Crop
}

type ManifestSource struct {
	Commit, URL, Provenance string
	State, Role, Theme      string
	Conditions              string
	Viewport                Viewport
}

type Viewport struct{ Width, Height int }
type Crop struct{ X, Y, Width, Height float64 }

type ManifestChange struct {
	ID, Title, Description string
	Status, Reason         string
	Files, AssetIDs        []string
}
