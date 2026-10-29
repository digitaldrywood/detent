package issuefiling

import "testing"

func TestOccurrenceRequiresCommentSupport(t *testing.T) {
	t.Parallel()
	if err := CommentOccurrence(t.Context(), nil, "issue", "body"); err == nil {
		t.Fatal("missing comment support accepted")
	}
}
