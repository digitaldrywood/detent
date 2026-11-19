package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	nativePreviewPageLimit   = 1 << 20
	nativePreviewPagePrefix  = `{"items":[`
	nativePreviewItemsBudget = nativePreviewPageLimit - len(`],"next":9223372036854775807}`)
)

var errPreviewInputsTooLarge = errors.New("claim preview scheduling inputs exceed the runner response limit")

type nativeClaimPreviewIssue struct {
	tracker.NativeIssue
	Blockers []hubrecords.NativeClaimBlocker `json:"blockers"`
}

type nativeClaimPreviewPage []byte

func encodePreviewIssue(issue nativeClaimPreviewIssue) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(issue); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (p nativeClaimPreviewPage) finish(after tracker.WorkItemID) ([]byte, error) {
	footer := `]}`
	if after != 0 {
		footer = `],"next":` + strconv.FormatInt(int64(after), 10) + "}"
	}
	if len(p)+len(footer) > nativePreviewPageLimit {
		return nil, errPreviewInputsTooLarge
	}
	return append(p, footer...), nil
}

func (p nativeClaimPreviewPage) add(issue nativeClaimPreviewIssue) (nativeClaimPreviewPage, bool, error) {
	encoded, err := encodePreviewIssue(issue)
	if err != nil {
		return nil, false, err
	}
	if len(nativePreviewPagePrefix)+len(encoded) > nativePreviewItemsBudget {
		issue.NativeIssue = schedulingPreviewIssue(issue.NativeIssue)
		encoded, err = encodePreviewIssue(issue)
		if err != nil {
			return nil, false, err
		}
	}
	if len(p) != len(nativePreviewPagePrefix) {
		if len(p)+1+len(encoded) > nativePreviewItemsBudget {
			return p, false, nil
		}
		p = append(p, ',')
	} else if len(p)+len(encoded)+len(`]}`) > nativePreviewPageLimit {
		return nil, false, errPreviewInputsTooLarge
	}
	return append(p, encoded...), true, nil
}

func schedulingPreviewIssue(issue tracker.NativeIssue) tracker.NativeIssue {
	projected := tracker.NativeIssue{
		NativeReference: issue.NativeReference, Title: issue.Title, Body: issue.Body, State: issue.State,
		Actor: tracker.Actor{PrincipalID: issue.Actor.PrincipalID}, IssueContract: issue.IssueContract,
		ReleaseDelivery: issue.ReleaseDelivery, ListChange: issue.ListChange,
		CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, ClosedAt: issue.ClosedAt, Terminal: issue.Terminal,
		Priority: issue.Priority, Labels: issue.Labels, Assignees: issue.Assignees,
		IgnoreDependencies: issue.IgnoreDependencies, DependencyReady: issue.DependencyReady, BlockersTruncated: issue.BlockersTruncated,
		Dependencies: issue.Dependencies, Dependents: issue.Dependents, OmittedFields: issue.OmittedFields,
	}
	if issue.Provenance != nil {
		projected.Provenance = &tracker.Provenance{AuthorID: issue.Provenance.AuthorID}
	}
	return projected
}
