package hubserver

import (
	"context"
	"net/url"
	"strconv"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type issueArchiveSection struct {
	OrganizationID   tracker.OrganizationID `json:"organization_id"`
	ProjectID        tracker.ProjectID      `json:"project_id"`
	WorkItemID       string                 `json:"work_item_id"`
	Revision         int64                  `json:"revision,string"`
	SectionID        string                 `json:"section_id"`
	SHA256           string                 `json:"sha256"`
	CompressedSize   int64                  `json:"compressed_size"`
	UncompressedSize int64                  `json:"uncompressed_size"`
	Codec            string                 `json:"codec"`
	SchemaVersion    int                    `json:"schema_version"`
	AccessPath       string                 `json:"access_path"`
}

func readIssueArchiveSection(ctx context.Context, q nativeQueryer, scope nativeScope, item, section string) (issueArchiveSection, error) {
	result := issueArchiveSection{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, SectionID: section}
	var key, kind, retention string
	err := q.QueryRowContext(ctx, `SELECT r.archive_revision,a.object_key,a.sha256,a.size,a.uncompressed_size,a.codec,a.schema_version,a.kind,a.retention_class
FROM issue_archive_sections r JOIN attachments a ON a.id=r.attachment_id AND a.organization_id=r.organization_id
WHERE r.organization_id=? AND r.project_id=? AND r.work_item_id=? AND r.section_id=?
AND r.archive_revision=(SELECT MAX(archive_revision) FROM issue_archive_sections WHERE organization_id=r.organization_id AND project_id=r.project_id AND work_item_id=r.work_item_id)`, scope.organization, scope.project, item, section).
		Scan(&result.Revision, &key, &result.SHA256, &result.CompressedSize, &result.UncompressedSize, &result.Codec, &result.SchemaVersion, &kind, &retention)
	if err != nil {
		return result, err
	}
	ref := attachment.ArchiveReference{Key: key, SHA256: result.SHA256, CompressedSize: result.CompressedSize, UncompressedSize: result.UncompressedSize, Codec: result.Codec, SchemaVersion: result.SchemaVersion, Kind: kind, Retention: retention}
	if ref.Validate(string(scope.organization)) != nil || result.SchemaVersion != 1 {
		return result, nativeNotFound()
	}
	result.AccessPath = "/api/v2/organizations/" + url.PathEscape(string(scope.organization)) + "/projects/" + url.PathEscape(string(scope.project)) + "/work-items/" + url.PathEscape(item) + "/archives/" + result.SHA256 + "/access?section=" + url.QueryEscape(section) + "&revision=" + strconv.FormatInt(result.Revision, 10)
	return result, nil
}
