package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) repairImportedDoctorProposalIdentities(ctx context.Context, record CoreWorkItemImport) error {
	if record.DoctorIdentities == nil {
		return nil
	}
	if err := s.LockWorkItemProject(ctx, record.Item.ProjectID); err != nil {
		return err
	}
	item, err := s.WorkItem(ctx, record.Item.ProjectID, record.Item.NativeID)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.Revision != record.Item.Revision || stringValue(item.BodyObjectKey) != stringValue(record.Item.BodyObjectKey) {
		return nil
	}
	version, err := s.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if version.PayloadSHA256 != record.DoctorIdentities.VersionSHA256 {
		return nil
	}
	return s.BindDoctorProposalIdentities(ctx, *record.DoctorIdentities)
}
