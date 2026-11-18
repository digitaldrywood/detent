package pgstoretest

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func PolicyTestRevision(organization tracker.OrganizationID, project tracker.ProjectID, policyID string, at time.Time) policystore.Revision {
	digestBytes := sha256.Sum256([]byte(string(organization) + "/" + string(project) + "/" + policyID))
	digest := hex.EncodeToString(digestBytes[:])
	return policystore.Revision{OrganizationID: organization, ProjectID: project, PolicyID: policyID, GitHubPullRequest: new(false), ApprovedBy: "admin", ApprovedAt: at, PayloadObjectKey: "orgs/" + string(organization) + "/blobs/sha256/" + digest[:2] + "/" + digest, PayloadSHA256: digest, PayloadBytes: 1, PayloadContentBytes: 1}
}
