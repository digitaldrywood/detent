package hubserver

import (
	"context"
	"net/url"
	"strconv"

	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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

func readIssueArchiveSection(ctx context.Context, reader attachmentstore.StorageArchiveReader, scope hubscope.NativeScope, item, section string) (issueArchiveSection, error) {
	result := issueArchiveSection{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, SectionID: section}
	record, err := reader.StorageArchiveSection(ctx, attachmentstore.StorageArchiveSectionQuery{ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(item), SectionID: section})
	if err != nil {
		return result, err
	}
	ref := record.Archive.Reference
	if ref.Validate(string(scope.Organization)) != nil || (ref.SchemaVersion != 1 && ref.SchemaVersion != 2) {
		return result, hubapi.NativeNotFound()
	}
	result.Revision = record.Stamp.Revision
	result.SHA256 = ref.SHA256
	result.CompressedSize = ref.CompressedSize
	result.UncompressedSize = ref.UncompressedSize
	result.Codec = ref.Codec
	result.SchemaVersion = ref.SchemaVersion
	result.AccessPath = "/api/v2/organizations/" + url.PathEscape(string(scope.Organization)) + "/projects/" + url.PathEscape(string(scope.Project)) + "/work-items/" + url.PathEscape(item) + "/archives/" + result.SHA256 + "/access?section=" + url.QueryEscape(section) + "&revision=" + strconv.FormatInt(result.Revision, 10)
	return result, nil
}
