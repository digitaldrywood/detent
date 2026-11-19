package executionapi

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/diffbody"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttemptDiffExpiredManifestDropsOnlyArchivedBodies(t *testing.T) {
	t.Parallel()
	files := []tracker.AttemptDiffFile{
		{Path: "example.go", Status: tracker.DiffStatusModified, Patch: "+inline", PatchBody: &diffbody.Reference{Key: "example/archive", SHA256: strings.Repeat("a", 64), Bytes: 7}},
		{Path: "expired.go", Status: tracker.DiffStatusModified, PatchExpired: true},
	}
	got := expireAttemptDiffBodies(files, true)
	if got[0].Patch != "+inline" || got[0].PatchBody != nil || !got[0].PatchExpired || !got[1].PatchExpired {
		t.Fatalf("expired manifest files=%+v", got)
	}
}
