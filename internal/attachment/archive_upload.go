package attachment

import (
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ArchiveLeaseHeader = "X-Detent-Lease"
const ArchiveFenceHeader = "X-Detent-Fencing-Token"

type ArchiveFence struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token,string"`
}

func (f ArchiveFence) Valid() bool {
	return validOrganization(string(f.LeaseID)) && f.FencingToken > 0
}

func (f ArchiveFence) Key(organization, project, item, digest string) (string, error) {
	key, err := ArchiveKey(organization, digest)
	if err != nil || !f.Valid() || !validOrganization(project) || !validOrganization(item) {
		return "", ErrInvalid
	}
	bound := artifact.Digest([]byte(strings.Join([]string{project, item, string(f.LeaseID), strconv.FormatInt(int64(f.FencingToken), 10)}, "\n")))
	return strings.TrimSuffix(key, digest) + bound + "/" + digest, nil
}

type ArchiveCheck struct {
	Reference *ArchiveReference `json:"reference,omitempty"`
	Fence     *ArchiveFence     `json:"fence,omitempty"`
}

type ArchivePublication struct {
	ArchiveReference
	Fence *ArchiveFence `json:"fence,omitempty"`
}

func (r ArchiveReference) ExecutionScoped(organization string) bool {
	plain, err := ArchiveKey(organization, r.SHA256)
	return err == nil && r.Key != plain && validArchiveBlobKey(organization, r.Key, r.SHA256)
}

const ArchiveDiscardObject = "discard_archive_object"
