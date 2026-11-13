package hubfixture

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Change struct {
	Native
	Issue  tracker.NativeIssue
	Change tracker.ChangeRequest
	Path   string
	Rules  tracker.ChangeReviewPolicy
}

func NewChange(t *testing.T, h *Hub, organizations ...tracker.OrganizationID) Change {
	t.Helper()
	return NewNamedChange(t, h, "changes", organizations...)
}

func NewNamedChange(t *testing.T, h *Hub, name string, organizations ...tracker.OrganizationID) Change {
	t.Helper()
	organization := tracker.OrganizationID("")
	if len(organizations) > 0 {
		organization = organizations[0]
	}
	return NewChangeIn(t, NewNative(t, h, organization, name))
}

func NewChangeIn(t *testing.T, p Native) Change {
	t.Helper()
	ApprovePolicy(t, p.Hub, p.Base+"/policy", Policy())
	credential, err := p.Credential(t, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	rules := tracker.ChangeReviewPolicy{PolicyID: Policy().ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: credential.ID, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
	response := p.Hub.Request(t, http.MethodPut, p.Base+"/change-review-policy", string(p.Hub.Config.InitialAdminToken), tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules})
	RequireStatus(t, response, http.StatusOK)
	Decode(t, response, &rules)
	issue := p.Create(t, "work")
	path := p.Base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response = p.Hub.Request(t, http.MethodPost, path, p.Token, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change", Body: "Proposed work"})
	RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	Decode(t, response, &change)
	return Change{Native: p, Issue: issue, Change: change, Path: path + "/" + change.ID, Rules: rules}
}

func NewChangeInOrganization(t *testing.T, h *Hub) Change {
	t.Helper()
	return NewChangeIn(t, NewNativeInOrganization(t, h))
}

func ChangeInput() tracker.ChangeVersionInput {
	return tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), Repository: "https://github.com/example/repo", Code: ChangeArtifact("code"), Artifacts: []tracker.ChangeArtifact{ChangeArtifact("manifest")}, PolicyID: Policy().ID}
}

func ChangeArtifact(kind string) tracker.ChangeArtifact {
	return tracker.ChangeArtifact{Kind: kind, URI: "s3://customer/change/" + kind, SHA256: policy.Digest([]byte(kind)), Availability: "available"}
}

func (c Change) Publish(t *testing.T, key, expected string) tracker.ChangeVersion {
	t.Helper()
	return c.PublishSource(t, key, expected, nil)
}

func (c Change) PublishSource(t *testing.T, key, expected string, bundle func(*tracker.ChangeVersionInput) []byte) tracker.ChangeVersion {
	t.Helper()
	input := ChangeInput()
	if expected != "" {
		input.HeadSHA = strings.Repeat("c", 40)
	}
	var source []byte
	if bundle != nil {
		source = bundle(&input)
	}
	response := c.Hub.Request(t, http.MethodPost, c.Path+"/versions", c.Token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedVersionID: expected, ChangeVersionInput: input, SourceBundle: source})
	RequireStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	Decode(t, response, &version)
	return version
}

func (c Change) Detail(t *testing.T) tracker.ChangeDetail {
	t.Helper()
	response := c.Hub.Request(t, http.MethodGet, c.Path, c.Token, nil)
	RequireStatus(t, response, http.StatusOK)
	var detail tracker.ChangeDetail
	Decode(t, response, &detail)
	response = c.Hub.Request(t, http.MethodGet, c.Path+"?section=versions", c.Token, nil)
	RequireStatus(t, response, http.StatusOK)
	var current tracker.ChangeDetail
	Decode(t, response, &current)
	if !reflect.DeepEqual(current.Summary, detail.Summary) {
		t.Fatalf("current summary = %#v, full summary = %#v, error = %v", current.Summary, detail.Summary, nil)
	}
	return detail
}

func CheckResult(version tracker.ChangeVersion) tracker.SubmitChangeCheck {
	check := version.Checks[0]
	return tracker.SubmitChangeCheck{Mutation: tracker.Mutation{IdempotencyKey: "check"}, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: check.CheckRunID, HeadSHA: version.HeadSHA, RunID: version.RunID, PolicyID: version.PolicyID, ConfigDigest: version.Policy.ConfigDigest, WorkflowID: check.WorkflowID, WorkflowSHA256: check.WorkflowSHA256, Source: check.Source, Conclusion: "success", CompletedAt: version.CreatedAt, Evidence: []tracker.ChangeArtifact{ChangeArtifact("test")}}}
}
