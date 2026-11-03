package tracker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNoChangeVersionRejectsMalformedSHA(t *testing.T) {
	for _, sha := range []string{"", "x", "x:y", "abc1234", strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40), " " + strings.Repeat("a", 40), strings.Repeat("a", 40) + "\n"} {
		t.Run(sha, func(t *testing.T) {
			n := NativeNoChangeCompletion{PolicyID: "policy", Repository: "repository", VerifiedSHA: strings.Repeat("b", 40), ReportedSHA: sha, Evidence: "evidence"}
			if got := n.VersionID(); got != "" {
				t.Fatalf("malformed SHA derived version ID %q", got)
			}
		})
	}
}

func TestNoChangeVersionFramesFields(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tt := range []struct {
		name        string
		left, right NativeNoChangeCompletion
	}{
		{"policy and repository", NativeNoChangeCompletion{PolicyID: "x", Repository: "y:z", VerifiedSHA: sha, ReportedSHA: sha, Evidence: "evidence"}, NativeNoChangeCompletion{PolicyID: "x:y", Repository: "z", VerifiedSHA: sha, ReportedSHA: sha, Evidence: "evidence"}},
		{"repository and verified SHA", NativeNoChangeCompletion{PolicyID: "policy", Repository: "x", VerifiedSHA: "y:z", ReportedSHA: sha, Evidence: "evidence"}, NativeNoChangeCompletion{PolicyID: "policy", Repository: "x:y", VerifiedSHA: "z", ReportedSHA: sha, Evidence: "evidence"}},
		{"different evidence", NativeNoChangeCompletion{PolicyID: "policy", Repository: "repository", VerifiedSHA: sha, ReportedSHA: sha, Evidence: "y:z"}, NativeNoChangeCompletion{PolicyID: "policy", Repository: "repository", VerifiedSHA: sha, ReportedSHA: sha, Evidence: "z"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			left, right := tt.left.VersionID(), tt.right.VersionID()
			if left == "" || right == "" || left == right {
				t.Fatalf("distinct tuples derived IDs %q and %q", left, right)
			}
		})
	}
	for _, sha := range []string{sha, strings.Repeat("B", 40), strings.Repeat("c", 64)} {
		t.Run("valid SHA "+sha, func(t *testing.T) {
			n := NativeNoChangeCompletion{ReportedSHA: sha}
			if n.VersionID() == "" {
				t.Fatal("full commit object ID rejected")
			}
		})
	}
}

func TestNoChangeLegacyReceiptRecovery(t *testing.T) {
	const legacyID = "version_842bc23c101da98833d79ae61e44ef12"
	var receipt NativeStepReceipt
	data := `{"step":"review","validator":{"version_id":"version_842bc23c101da98833d79ae61e44ef12","verdict":"pass","submitted":true,"session_id":11}}`
	if err := json.Unmarshal([]byte(data), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Validator == nil || receipt.Validator.VersionID != legacyID || receipt.Validator.Verdict != "pass" || receipt.Validator.SessionID != 11 {
		t.Fatalf("legacy receipt lost: %+v", receipt.Validator)
	}
}
