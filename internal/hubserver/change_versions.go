package hubserver

import (
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateChangeVersion(input tracker.ChangeVersionInput) error {
	if len(input.ReviewDispositions) > 100 {
		return nativePublicInvalid("Review dispositions must contain at most 100 threads")
	}
	seenThreads := make(map[string]bool, len(input.ReviewDispositions))
	for _, d := range input.ReviewDispositions {
		if d.HeadSHA != input.HeadSHA || strings.TrimSpace(d.ThreadID) == "" || len(d.ThreadID) > 256 || seenThreads[d.ThreadID] || d.Disposition != "fixed" && d.Disposition != "dismissed" || strings.TrimSpace(d.Evidence) == "" || len(d.Evidence) > 8192 {
			return nativePublicInvalid("Review dispositions require the published head, unique thread IDs, fixed or dismissed decisions and bounded evidence")
		}
		seenThreads[d.ThreadID] = true
	}
	if v := input.Validation; v != nil && (v.HeadSHA != input.HeadSHA || v.ExitCode != 0 || v.DurationNS <= 0 || strings.TrimSpace(v.Command) == "" || len(v.Command) > 4096 || len(v.Output) > 64*1024 || !validCommitID(v.TreeSHA) || !v.Evidence.Valid()) {
		return nativeRejection(nativePublicInvalid("Validation receipt must cover the published head and identify a successful command"), "version_validation", "validation")
	}
	for _, commit := range []string{input.BaseSHA, input.HeadSHA, input.MergeBaseSHA} {
		if !changerequest.ValidHash(commit, 40) && !changerequest.ValidHash(commit, 64) {
			return nativePublicInvalid("Base, head and merge-base must be lowercase commit identities")
		}
	}
	if !changerequest.ValidReference(input.Repository) || input.Code.Kind != "code" {
		return nativePublicInvalid("Repository and code artifact references are required")
	}
	if err := changerequest.ValidateArtifacts(append([]tracker.ChangeArtifact{input.Code}, input.Artifacts...)); err != nil {
		return nativeInvalid(err.Error())
	}
	if (input.RunID == "") != (input.AttemptID == "") || input.RunID != "" && (!validNativeID(input.RunID, "run") || !validNativeID(input.AttemptID, "attempt")) {
		return nativePublicInvalid("Run and attempt identities must be supplied together")
	}
	if input.External != nil {
		u, err := url.Parse(input.External.URL)
		if err != nil || u == nil || input.External.Provider != "github" || input.External.ID == "" || len(input.External.ID) > 128 || !changerequest.ValidReference(input.External.URL) || u.Scheme != "https" || u.Host != "github.com" {
			return nativePublicInvalid("External PR reference must identify a GitHub pull request")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "pull" || parts[3] != input.External.ID {
			return nativePublicInvalid("External PR identity and URL must agree")
		}
	}
	return nil
}

func validateChangeVersionEvidence(scope nativeScope, item string, request tracker.PublishChangeVersion) error {
	validation := request.Validation
	if validation == nil || validation.EvidenceArtifact == nil {
		return nil
	}
	ref := validation.EvidenceArtifact
	if ref.Validate() != nil || ref.SchemaVersion != 1 || !artifact.ValidID(ref.ServiceID, "service") || !artifact.ValidID(ref.ArtifactID, "artifact") || !artifact.ValidID(ref.ManifestID, "manifest") || !artifact.ValidHash(ref.SHA256, 64) || ref.Revision < 1 || ref.Kind != "log" || ref.State != "complete" || ref.Availability != "available" || ref.Bytes <= 0 || ref.Bytes > artifact.MaxArtifactBytes || ref.Objects < 1 || ref.Objects > artifact.MaxObjects || ref.ExpiresAt.IsZero() || ref.ObservedAt.IsZero() || ref.OrganizationID != string(scope.organization) || ref.ProjectID != string(scope.project) || ref.WorkItemID != item || ref.RunID != request.RunID || ref.AttemptID != request.AttemptID {
		return nativeRejection(nativeInvalid("Validation artifact must identify complete evidence from the publishing attempt"), "version_validation", "validation")
	}
	return nil
}

func (s *Service) publishChangeVersion(c echo.Context) error {
	return s.publishChangeVersionRecord(c)
}

type preparedChangeVersion struct {
	change     tracker.ChangeRequest
	version    tracker.ChangeVersion
	raw        string
	associated bool
	delivery   tracker.ReleaseDelivery
	source     []byte
	sourceRef  attachment.ArchiveReference
	session    changeSessionAuthority
}
