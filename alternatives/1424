package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	Detail(context.Context, tracker.NativeWorkItemID, http.Header) (tracker.NativeRecovery, error)
	Evidence(context.Context, tracker.NativeWorkItemID, string) (tracker.NativeRecoveryPage, error)
}

func Load(ctx context.Context, reader Reader, organization tracker.OrganizationID, project tracker.ProjectID, id tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	result, err := reader.Detail(ctx, id, http.Header{"X-Detent-Recovery-Detail": {"full"}, "X-Detent-Recovery-Projection": {"bounded"}, "X-Detent-Recovery-Body": {"fragments"}})
	if err == nil && result.Continuation != nil {
		return tracker.NativeRecovery{}, errors.New("read recovery: unbounded detail continuation is not candidate hydration")
	}
	if err == nil && (result.Issue.OrganizationID != organization || result.Issue.ProjectID != project || result.Issue.WorkItemID != id) {
		return result, errors.New("read recovery: scoped work item is missing")
	}
	if err == nil && result.Read != nil && result.Read.IssueBody != nil {
		err = restoreBody(ctx, reader, &result)
	} else if err == nil && result.Read != nil && slices.Contains(result.Read.Omitted, "issue.body") {
		err = errors.New("read recovery: required issue body reference is missing")
	}
	if err != nil {
		return tracker.NativeRecovery{}, err
	}
	return result, err
}

func restoreBody(ctx context.Context, reader Reader, recovery *tracker.NativeRecovery) error {
	read := recovery.Read.IssueBody
	const maxBody = 256 << 10
	if recovery.Issue.Body != "" || recovery.Issue.Revision <= 0 || read.Cursor == "" || len(read.SHA256) != 64 || read.TotalBytes < 2 || read.TotalBytes > 6*maxBody+2 {
		return errors.New("read recovery: invalid required issue body reference")
	}
	var body []byte
	cursor := read.Cursor
	for int64(len(body)) < read.TotalBytes {
		page, err := reader.Evidence(ctx, recovery.Issue.WorkItemID, cursor)
		if err != nil {
			return err
		}
		if page.Section != "issue_body" || page.RecordID != string(recovery.Issue.WorkItemID) || page.Revision != recovery.Issue.Revision || page.SHA256 != read.SHA256 || page.TotalBytes != read.TotalBytes || page.Offset != int64(len(body)) || len(page.Data) == 0 || len(page.Data) > 64<<10 {
			return errors.New("read recovery: required issue body changed or fragment is invalid")
		}
		body = append(body, page.Data...)
		if int64(len(body)) < read.TotalBytes && (page.NextCursor == "" || page.NextCursor == cursor) {
			return errors.New("read recovery: required issue body did not advance")
		}
		cursor = page.NextCursor
	}
	digest := sha256.Sum256(body)
	var restored string
	if hex.EncodeToString(digest[:]) != read.SHA256 || json.Unmarshal(body, &restored) != nil || len(restored) > maxBody {
		return errors.New("read recovery: required issue body digest or content is invalid")
	}
	recovery.Issue.Body = restored
	return nil
}
