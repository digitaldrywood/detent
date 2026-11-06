package attachment

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/digitaldrywood/detent/internal/conversation"
)

var referencePattern = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\((/organizations/(org_[A-Za-z0-9_-]+)/api/v2/projects/(prj_[A-Za-z0-9_-]+)/attachments/(att_[a-f0-9]{32}))\)`)

type Reference struct {
	URL   string
	ID    string
	Image bool
}

func URIReference(value, organization, project string) (Reference, bool) {
	if len(value) > 2048 {
		return Reference{}, false
	}
	if strings.HasPrefix(value, "attachment:") {
		id := strings.TrimPrefix(value, "attachment:")
		return Reference{URL: value, ID: id}, conversation.ValidateAttachmentID(id) == nil
	}
	wrapped := "[](" + value + ")"
	match := referencePattern.FindStringSubmatch(wrapped)
	if len(match) == 0 || match[0] != wrapped || organization != "" && match[4] != organization || project != "" && match[5] != project {
		return Reference{}, false
	}
	return Reference{URL: value, ID: match[6]}, true
}

func (m Metadata) Markdown(organization string) string {
	name := url.PathEscape(m.Name)
	path := "/organizations/" + url.PathEscape(organization) + "/api/v2/projects/" + url.PathEscape(m.ProjectID) + "/attachments/" + m.ID
	prefix := ""
	if strings.HasPrefix(m.ContentType, "image/") {
		prefix = "!"
	}
	return prefix + "[" + name + "](" + path + ")"
}

func References(body, organization, project string) []Reference {
	var refs []Reference
	for _, match := range referencePattern.FindAllStringSubmatch(body, -1) {
		if match[4] != organization || match[5] != project {
			continue
		}
		refs = append(refs, Reference{URL: match[3], ID: match[6], Image: match[1] == "!"})
	}
	return refs
}
