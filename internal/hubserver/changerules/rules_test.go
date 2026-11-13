package changerules_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReviewInstructionsStayWithinTheCommentLimit(t *testing.T) {
	t.Parallel()
	change := tracker.ChangeRequest{ID: "change_1"}
	review := tracker.ChangeReview{VersionID: "version_1", Body: strings.Repeat("é", 40<<10)}
	body := changerules.ReviewInstructions(change, review)
	if len(body) > changerules.MaxCommentBytes || !utf8.ValidString(body) || !strings.HasPrefix(body, "Changes requested on Change Request change_1 (version version_1).\n\n") {
		t.Fatalf("instructions = %d bytes, valid = %t", len(body), utf8.ValidString(body))
	}
	if short := changerules.ReviewInstructions(change, tracker.ChangeReview{Body: " Move it. "}); short != "Changes requested on Change Request change_1.\n\nMove it." {
		t.Fatalf("short instructions = %q", short)
	}
}
