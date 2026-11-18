package hubserver

import (
	"strconv"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeSourceReferences(nodeID, owner, repository string, number int, sourceURL, importedURL string, source *tracker.Provenance) []tracker.ExternalReference {
	references := []tracker.ExternalReference{}
	if sourceURL != "" {
		references = append(references, tracker.GitHubIssueSourceReference(sourceURL, sourceURL))
	}
	if number > 0 && owner != "" && repository != "" {
		sourceURL = "https://github.com/" + owner + "/" + repository + "/issues/" + strconv.Itoa(number)
	} else if sourceURL == "" {
		sourceURL = importedURL
	}
	if source != nil {
		if source.Provider == "github" {
			references = append(references, tracker.GitHubIssueSourceReference(source.ExternalID, sourceURL))
		} else {
			references = append(references, tracker.ExternalReference{Provider: source.Provider, Kind: "issue", ID: source.ExternalID})
		}
	} else if nodeID != "" || number > 0 {
		if nodeID == "" {
			nodeID = sourceURL
		}
		references = append(references, tracker.GitHubIssueSourceReference(nodeID, sourceURL))
	}
	return references
}
