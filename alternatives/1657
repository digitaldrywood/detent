package changerules

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// LandingLane is the lane a reviewed Change Request waits in for the runner
// that lands it. It is dispatchable so the runner claims the item, and it is
// named rather than inferred so a workflow without one keeps its reviewed
// changes in review, where a person lands them by hand.
const LandingLane = "Merging"

// LandingTarget is the lane an approved review moves the primary issue to:
// the landing lane, when the issue's current lane may move there. It reports
// "" when the workflow has no such move.
func LandingTarget(project tracker.NativeProject, current string) string {
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name == name && strings.EqualFold(target.Name, LandingLane) && target.Dispatchable && !target.Terminal && !target.OperatorOnly {
					return target.Name
				}
			}
		}
	}
	return ""
}

// TerminalTarget is the lane a landed issue finishes in: the first terminal
// lane the current lane may move to.
func TerminalTarget(project tracker.NativeProject, current string) string {
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name == name && target.Terminal {
					return target.Name
				}
			}
		}
	}
	return ""
}

// ReworkTarget is the lane a request for changes moves the primary issue
// to: a dispatchable, non-terminal lane the current lane may move to, where
// the runner picks the item up again. "In Progress" is preferred when the
// workflow has it, so the item resumes rather than restarts.
func ReworkTarget(project tracker.NativeProject, current string) string {
	first := ""
	for _, state := range project.States {
		if state.Name != current {
			continue
		}
		for _, name := range state.Transitions {
			for _, target := range project.States {
				if target.Name != name || !target.Dispatchable || target.Terminal || target.OperatorOnly {
					continue
				}
				if strings.EqualFold(target.Name, "In Progress") {
					return target.Name
				}
				if first == "" {
					first = target.Name
				}
			}
		}
	}
	return first
}

// MaxCommentBytes is the comment body limit createNativeComment
// enforces; the generated header counts against it.
const MaxCommentBytes = 64 << 10

// ReviewInstructions is the comment a request for changes leaves on the
// issue: a header naming the change and version, then the review text, cut
// at a character boundary so the whole comment stays within the limit a
// comment may be edited under.
func ReviewInstructions(change tracker.ChangeRequest, review tracker.ChangeReview) string {
	body := "Changes requested on Change Request " + change.ID
	if review.VersionID != "" {
		body += " (version " + review.VersionID + ")"
	}
	body += "."
	text := strings.TrimSpace(review.Body)
	if text == "" {
		return body
	}
	body += "\n\n"
	room := MaxCommentBytes - len(body)
	if len(text) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	return body + text
}

func LandingReady(state string, detail *tracker.ChangeDetail) bool {
	if !strings.EqualFold(strings.TrimSpace(state), "Merging") {
		return true
	}
	return detail != nil && detail.Change.CurrentLanding() == nil && detail.Change.CurrentVersion != "" && detail.Summary.Status == "reviewed"
}

func LandingClaimable(state string, detail *tracker.ChangeDetail) bool {
	if LandingReady(state, detail) {
		return true
	}
	return detail != nil && detail.Change.CurrentLanding() == nil && detail.Change.CurrentVersion != "" && detail.Summary.Status == "needs_evidence" &&
		slices.Contains([]string{"approved", "not_required", "stale"}, detail.Summary.NativeReview) &&
		slices.Contains([]string{"success", "not_required", "stale"}, detail.Summary.Checks)
}

func ValidateVersion(input tracker.ChangeVersionInput) error {
	if len(input.ReviewDispositions) > 100 {
		return hubapi.PublicInvalid("Review dispositions must contain at most 100 threads")
	}
	seenThreads := make(map[string]bool, len(input.ReviewDispositions))
	for _, d := range input.ReviewDispositions {
		if d.HeadSHA != input.HeadSHA || strings.TrimSpace(d.ThreadID) == "" || len(d.ThreadID) > 256 || seenThreads[d.ThreadID] || d.Disposition != "fixed" && d.Disposition != "dismissed" || strings.TrimSpace(d.Evidence) == "" || len(d.Evidence) > 8192 {
			return hubapi.PublicInvalid("Review dispositions require the published head, unique thread IDs, fixed or dismissed decisions and bounded evidence")
		}
		seenThreads[d.ThreadID] = true
	}
	if v := input.Validation; v != nil && (v.HeadSHA != input.HeadSHA || v.ExitCode != 0 || v.DurationNS <= 0 || strings.TrimSpace(v.Command) == "" || len(v.Command) > 4096 || len(v.Output) > 64*1024 || !hubapi.ValidCommitID(v.TreeSHA) || !v.Evidence.Valid()) {
		return hubapi.Rejection(hubapi.PublicInvalid("Validation receipt must cover the published head and identify a successful command"), "version_validation", "validation")
	}
	for _, commit := range []string{input.BaseSHA, input.HeadSHA, input.MergeBaseSHA} {
		if !changerequest.ValidHash(commit, 40) && !changerequest.ValidHash(commit, 64) {
			return hubapi.PublicInvalid("Base, head and merge-base must be lowercase commit identities")
		}
	}
	if !changerequest.ValidReference(input.Repository) || input.Code.Kind != "code" {
		return hubapi.PublicInvalid("Repository and code artifact references are required")
	}
	if err := changerequest.ValidateArtifacts(append([]tracker.ChangeArtifact{input.Code}, input.Artifacts...)); err != nil {
		return hubapi.Invalid(err.Error())
	}
	if (input.RunID == "") != (input.AttemptID == "") || input.RunID != "" && (!hubapi.ValidID(input.RunID, "run") || !hubapi.ValidID(input.AttemptID, "attempt")) {
		return hubapi.PublicInvalid("Run and attempt identities must be supplied together")
	}
	if input.External != nil {
		u, err := url.Parse(input.External.URL)
		if err != nil || u == nil || input.External.Provider != "github" || input.External.ID == "" || len(input.External.ID) > 128 || !changerequest.ValidReference(input.External.URL) || u.Scheme != "https" || u.Host != "github.com" {
			return hubapi.PublicInvalid("External PR reference must identify a GitHub pull request")
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "pull" || parts[3] != input.External.ID {
			return hubapi.PublicInvalid("External PR identity and URL must agree")
		}
	}
	return nil
}

func ValidateVersionEvidence(organization tracker.OrganizationID, project tracker.ProjectID, item string, request tracker.PublishChangeVersion) error {
	validation := request.Validation
	if validation == nil || validation.EvidenceArtifact == nil {
		return nil
	}
	ref := validation.EvidenceArtifact
	if ref.Validate() != nil || ref.SchemaVersion != 1 || !artifact.ValidID(ref.ServiceID, "service") || !artifact.ValidID(ref.ArtifactID, "artifact") || !artifact.ValidID(ref.ManifestID, "manifest") || !artifact.ValidHash(ref.SHA256, 64) || ref.Revision < 1 || ref.Kind != "log" || ref.State != "complete" || ref.Availability != "available" || ref.Bytes <= 0 || ref.Bytes > artifact.MaxArtifactBytes || ref.Objects < 1 || ref.Objects > artifact.MaxObjects || ref.ExpiresAt.IsZero() || ref.ObservedAt.IsZero() || ref.OrganizationID != string(organization) || ref.ProjectID != string(project) || ref.WorkItemID != item || ref.RunID != request.RunID || ref.AttemptID != request.AttemptID {
		return hubapi.Rejection(hubapi.Invalid("Validation artifact must identify complete evidence from the publishing attempt"), "version_validation", "validation")
	}
	return nil
}

func SecurityAuditRejection(message, field string) error {
	rejection := runnerauth.PublicationRejection{Category: "security_audit", Field: field}
	if !rejection.Valid() {
		rejection.Field = "review"
	}
	return &hubapi.Error{Code: "invalid_request", Message: message, Status: http.StatusUnprocessableEntity, Details: map[string]any{"rejection": rejection}}
}

func SecurityAuditEvaluationField(evaluation securityaudit.Evaluation) string {
	if evaluation.Reason == securityaudit.ReasonDiffTooLarge {
		return "run.diff_bytes"
	}
	field, _, _ := strings.Cut(evaluation.Detail, " ")
	if (runnerauth.PublicationRejection{Category: "security_audit", Field: field}).Valid() {
		return field
	}
	return "run"
}

var releaseBranchPattern = regexp.MustCompile(`^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func ValidateReleasePick(r tracker.AssociateReleasePick) error {
	if r.PRNumber < 1 {
		return hubapi.PublicInvalid("Release association requires an existing PR number")
	}
	return ValidatePreparedReleasePick(r.ReleasePickInput, r.ExpectedRevision)
}

func ValidatePreparedReleasePick(r tracker.ReleasePickInput, revision tracker.Revision) error {
	if r.HeadSHA == r.BaseSHA || r.HeadRef == r.BaseRef || r.SourceMergeSHA == r.FrozenSHA {
		return hubapi.PublicInvalid("Release pick must identify a distinct source fix and PR head")
	}
	if !hubapi.ValidID(string(r.SourceIssueID), "wi") || !hubapi.ValidID(r.SourceChangeID, "change") || revision < 1 || !releaseBranchPattern.MatchString(r.BaseRef) || !ValidReleaseHeadRef(r.HeadRef) {
		return hubapi.PublicInvalid("Release delivery requires a source issue and Change, revision, pick branch and frozen release branch")
	}
	for _, sha := range []string{r.SourceMergeSHA, r.HeadSHA, r.BaseSHA, r.FrozenSHA} {
		if !hubapi.ValidCommitID(sha) {
			return hubapi.PublicInvalid("Release delivery requires exact commit identities")
		}
	}
	return nil
}

func ValidReleaseHeadRef(ref string) bool {
	return ref != "" && len(ref) <= 256 && !strings.HasPrefix(ref, "-") && !strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock") && !strings.Contains(ref, "..") && !strings.Contains(ref, "@{") && !strings.ContainsAny(ref, " ~^:?*[\\\t\r\n")
}

func SameReleasePickDelivery(prior, next tracker.ReleasePickInput) bool {
	next.BaseSHA, next.HeadSHA = prior.BaseSHA, prior.HeadSHA
	return prior == next
}

func ReleaseDeliverySectionsMatch(body, approved string) bool {
	if approved == "" {
		return false
	}
	actual := config.IssueContractSectionDigests(body)
	for section, digest := range config.IssueContractSectionDigests(approved) {
		if actual[section] != digest {
			return false
		}
	}
	return true
}

func ApproveReleaseDeliveryContract(issue *tracker.NativeIssue, approved policy.Descriptor, now time.Time) error {
	if _, machine := issueorigin.Parse(issue.Body); !machine || approved.Authored == nil {
		return nil
	}
	if issue.IssueContract == nil {
		issue.IssueContract = &issuecontract.State{}
	}
	if len(issue.IssueContract.ConfirmedSections) > 0 || issue.IssueContract.HumanAction != "" || issue.State == "Blocked" || strings.Contains(strings.ToLower(issue.Body), "detent:no-dispatch") {
		return nil
	}
	contract, err := config.ResolvePolicyIssueContract(approved)
	if err != nil {
		return err
	}
	body := contract.ReleaseDeliveryBody()
	if !ReleaseDeliverySectionsMatch(issue.Body, body) {
		return nil
	}
	issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(body)
	issue.IssueContract.ReleaseDelivery = &issuecontract.ReleaseDeliveryApproval{PolicyID: approved.ID, BodySHA256: policy.Digest([]byte(issue.Body))}
	issue.IssueContract.RecordedAt = now
	return nil
}

type ReleaseDeliveryDelegation struct {
	PolicyID string `json:"policy_id"`
	Body     string `json:"body"`
}
