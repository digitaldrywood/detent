package postgres

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrDoctorProposalIdentitiesPending = errors.New("doctor proposal identities are pending")

var doctorProposalMarker = regexp.MustCompile(`<!-- detent:self-improvement proposal_id=[^\s<>]+ -->`)

type DoctorProposalIdentity struct {
	Kind  string
	Value string
}

type DoctorProposalIdentityBinding struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	Revision       int64
	VersionSHA256  string
	Identities     []DoctorProposalIdentity
}

type DoctorProposalIdentitySelection struct {
	ProjectID tracker.ProjectID
	Identity  DoctorProposalIdentity
	OpenOnly  bool
}

type DoctorProposalIdentityResult struct {
	ProjectID      tracker.ProjectID
	SourceSequence int64
	WorkItemID     tracker.NativeWorkItemID
	Number         int64
	Revision       int64
	VersionSHA256  string
	Terminal       bool
}

type DoctorProposalIdentityHead struct {
	Item    corestore.WorkItem
	Version eventstore.CollaborationVersion
}

type DoctorProposalIdentityReader interface {
	tenantstore.Reader
	LookupDoctorProposalIdentity(context.Context, DoctorProposalIdentitySelection) (DoctorProposalIdentityResult, error)
	NextDoctorProposalIdentityHead(context.Context) (DoctorProposalIdentityHead, error)
}

type DoctorProposalIdentityWriter interface {
	tenantstore.Writer
	DoctorProposalInitializer
	DoctorProposalIdentityReader
	BindDoctorProposalIdentities(context.Context, DoctorProposalIdentityBinding) error
	ValidateDoctorProposalIdentitySelection(context.Context, DoctorProposalIdentityResult) error
}

func DeriveDoctorProposalIdentities(body string) []DoctorProposalIdentity {
	var identities []DoctorProposalIdentity
	for _, marker := range doctorProposalMarker.FindAllString(body, -1) {
		identities = append(identities, DoctorProposalIdentity{Kind: "marker", Value: marker})
	}
	if origin, ok := issueorigin.Parse(body); ok {
		identities = append(identities, DoctorProposalIdentity{Kind: "fingerprint", Value: origin.Fingerprint})
	}
	sortDoctorProposalIdentities(identities)
	return slices.Compact(identities)
}

func sortDoctorProposalIdentities(identities []DoctorProposalIdentity) {
	slices.SortFunc(identities, func(a, b DoctorProposalIdentity) int {
		if order := strings.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		return strings.Compare(a.Value, b.Value)
	})
}

func validDoctorProposalIdentity(identity DoctorProposalIdentity) bool {
	switch identity.Kind {
	case "fingerprint":
		return strings.TrimSpace(identity.Value) != ""
	case "marker":
		return doctorProposalMarker.FindString(identity.Value) == identity.Value && identity.Value != ""
	default:
		return false
	}
}

func (r *Repository) ReadDoctorProposalIdentities(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(DoctorProposalIdentityReader) error) error {
	if read == nil {
		return corestore.ErrInvalidPage
	}
	return r.transaction(ctx, organization, workload, true, func(s *session) error { return read(s) })
}

func (r *Repository) WriteDoctorProposalIdentities(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(DoctorProposalIdentityWriter) error) error {
	if write == nil {
		return corestore.ErrInvalidPage
	}
	return r.transaction(ctx, organization, workload, false, func(s *session) error { return write(s) })
}

func (s *session) LookupDoctorProposalIdentity(ctx context.Context, selection DoctorProposalIdentitySelection) (DoctorProposalIdentityResult, error) {
	if selection.ProjectID == "" || !validDoctorProposalIdentity(selection.Identity) {
		return DoctorProposalIdentityResult{}, eventstore.ErrInvalidRecord
	}
	row, err := s.queries.LookupDoctorProposalIdentity(ctx, pgquery.LookupDoctorProposalIdentityParams{OrganizationID: s.organization, ProjectID: selection.ProjectID, Kind: selection.Identity.Kind, IdentityHash: artifact.Digest([]byte(selection.Identity.Value)), IdentityValue: []byte(selection.Identity.Value), OpenOnly: selection.OpenOnly})
	if err != nil {
		return DoctorProposalIdentityResult{}, mapError(ctx, err)
	}
	if !row.Complete {
		return DoctorProposalIdentityResult{}, ErrDoctorProposalIdentitiesPending
	}
	return DoctorProposalIdentityResult{ProjectID: selection.ProjectID, SourceSequence: row.SourceSequence, WorkItemID: tracker.NativeWorkItemID(row.WorkItemID), Number: row.ItemNumber, Revision: row.SourceRevision, VersionSHA256: row.VersionSha256, Terminal: row.Terminal}, nil
}

func (s *session) NextDoctorProposalIdentityHead(ctx context.Context) (DoctorProposalIdentityHead, error) {
	row, err := s.queries.NextDoctorProposalIdentityHead(ctx, s.organization)
	if err != nil {
		return DoctorProposalIdentityHead{}, mapError(ctx, err)
	}
	item, err := s.WorkItem(ctx, row.ProjectID, row.NativeID)
	if errors.Is(err, corestore.ErrNotFound) {
		return DoctorProposalIdentityHead{}, corestore.ErrSerializationFailure
	}
	if err != nil {
		return DoctorProposalIdentityHead{}, err
	}
	version, err := s.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
	if errors.Is(err, corestore.ErrNotFound) {
		return DoctorProposalIdentityHead{}, eventstore.ErrInvalidRecord
	}
	return DoctorProposalIdentityHead{Item: item, Version: version}, err
}

func (s *session) ValidateDoctorProposalIdentitySelection(ctx context.Context, result DoctorProposalIdentityResult) error {
	if result.ProjectID == "" || result.SourceSequence < 0 {
		return eventstore.ErrInvalidRecord
	}
	if err := s.LockWorkItemProject(ctx, result.ProjectID); err != nil {
		return err
	}
	sequence, err := s.queries.DoctorProposalIdentitySource(ctx, pgquery.DoctorProposalIdentitySourceParams{OrganizationID: s.organization, ID: result.ProjectID})
	if err != nil {
		return mapError(ctx, err)
	}
	if sequence != result.SourceSequence {
		return corestore.ErrSerializationFailure
	}
	return nil
}

func (s *session) BindDoctorProposalIdentities(ctx context.Context, binding DoctorProposalIdentityBinding) error {
	if err := s.checkOrganization(binding.OrganizationID); err != nil {
		return err
	}
	if binding.ProjectID == "" || binding.WorkItemID == "" || binding.Revision < 1 || !artifact.ValidHash(binding.VersionSHA256, 64) {
		return eventstore.ErrInvalidRecord
	}
	identities := slices.Clone(binding.Identities)
	for _, identity := range identities {
		if !validDoctorProposalIdentity(identity) {
			return eventstore.ErrInvalidRecord
		}
	}
	sortDoctorProposalIdentities(identities)
	identities = slices.Compact(identities)
	identities = append(identities, DoctorProposalIdentity{Kind: "complete"})
	slices.SortFunc(identities, func(a, b DoctorProposalIdentity) int {
		if order := strings.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		return strings.Compare(artifact.Digest([]byte(a.Value)), artifact.Digest([]byte(b.Value)))
	})
	if err := s.LockWorkItemProject(ctx, binding.ProjectID); err != nil {
		return err
	}
	item, err := s.LockWorkItem(ctx, binding.ProjectID, binding.WorkItemID)
	if err != nil {
		return err
	}
	if item.Revision != binding.Revision {
		return corestore.ErrRevisionConflict
	}
	version, err := s.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
	if err != nil {
		return err
	}
	if version.PayloadSHA256 != binding.VersionSHA256 {
		return corestore.ErrRevisionConflict
	}
	if item.DoctorIdentityRevision != nil && *item.DoctorIdentityRevision == item.Revision {
		rows, err := s.queries.DoctorProposalIdentityRows(ctx, pgquery.DoctorProposalIdentityRowsParams{OrganizationID: s.organization, ProjectID: item.ProjectID, WorkItemID: string(item.NativeID)})
		if err != nil {
			return mapError(ctx, err)
		}
		if len(rows) != len(identities) {
			return mutation.ErrConflict
		}
		for i, row := range rows {
			identity := identities[i]
			if row.Kind != identity.Kind || row.IdentityHash != artifact.Digest([]byte(identity.Value)) || !bytes.Equal(row.IdentityValue, []byte(identity.Value)) || row.SourceRevision != item.Revision || row.VersionSha256 != version.PayloadSHA256 {
				return mutation.ErrConflict
			}
		}
		return nil
	}
	if err := s.queries.ClearDoctorProposalIdentities(ctx, pgquery.ClearDoctorProposalIdentitiesParams{OrganizationID: s.organization, ProjectID: item.ProjectID, WorkItemID: string(item.NativeID)}); err != nil {
		return mapError(ctx, err)
	}
	for _, identity := range identities {
		if err := s.queries.PutDoctorProposalIdentity(ctx, pgquery.PutDoctorProposalIdentityParams{OrganizationID: s.organization, ProjectID: item.ProjectID, WorkItemID: string(item.NativeID), Kind: identity.Kind, IdentityHash: artifact.Digest([]byte(identity.Value)), IdentityValue: []byte(identity.Value), SourceRevision: item.Revision, VersionSha256: version.PayloadSHA256, HasState: item.WorkflowStateName != nil, StateName: stringValue(item.WorkflowStateName), Archived: item.Archived, ItemNumber: item.Number}); err != nil {
			return mapError(ctx, err)
		}
	}
	changed, err := s.queries.BindDoctorProposalIdentityHead(ctx, pgquery.BindDoctorProposalIdentityHeadParams{OrganizationID: s.organization, ProjectID: item.ProjectID, NativeID: item.NativeID, Revision: item.Revision})
	if err != nil {
		return mapError(ctx, err)
	}
	if changed != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
