package attachment

import (
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ArchiveLeaseHeader = "X-Detent-Lease"
const ArchiveFenceHeader = "X-Detent-Fencing-Token"
const ArchiveUploadTimeout = time.Minute

type ArchiveFence struct {
	LeaseID      tracker.LeaseID      `json:"lease_id"`
	FencingToken tracker.FencingToken `json:"fencing_token,string"`
}

func (f ArchiveFence) Valid() bool {
	return validOrganization(string(f.LeaseID)) && f.FencingToken > 0
}

func (f ArchiveFence) Key(organization, project, item, binding, digest string) (string, error) {
	if binding != "" && !validOrganization(binding) {
		return "", ErrInvalid
	}
	return f.key(organization, project, item, digest, binding)
}

func (f ArchiveFence) MatchesPublished(organization, project, item string, ref ArchiveReference) bool {
	key, err := f.Key(organization, project, item, ref.BindingID, ref.SHA256)
	if err != nil {
		return false
	}
	if key == ref.Key {
		return true
	}
	legacy, err := f.key(organization, project, item, ref.SHA256)
	return err == nil && legacy == ref.Key
}

func (f ArchiveFence) key(organization, project, item, digest string, binding ...string) (string, error) {
	key, err := ArchiveKey(organization, digest)
	if err != nil || !f.Valid() || !validOrganization(project) || !validOrganization(item) {
		return "", ErrInvalid
	}
	parts := append([]string{project, item, string(f.LeaseID), strconv.FormatInt(int64(f.FencingToken), 10)}, binding...)
	bound := artifact.Digest([]byte(strings.Join(parts, "\n")))
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

type DeletionScan struct {
	Archives bool `json:"archives,omitempty"`
}

type ArchiveDeletionTarget struct {
	Key           string `json:"key"`
	BindingID     string `json:"binding_id,omitempty"`
	CustomerOwned bool   `json:"customer_owned,omitempty"`
}

func (target ArchiveDeletionTarget) Valid(organization string) bool {
	if target.BindingID == "" && target.CustomerOwned || target.BindingID != "" && !validOrganization(target.BindingID) {
		return false
	}
	ref := ArchiveReference{Key: target.Key, SHA256: target.Key[strings.LastIndex(target.Key, "/")+1:]}
	return ref.ExecutionScoped(organization)
}

type AttachmentDeletionReceipt struct {
	ID      string                 `json:"id"`
	Archive *ArchiveDeletionTarget `json:"archive,omitempty"`
}
