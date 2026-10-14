package attachment

import (
	"net/url"
	"regexp"
	"strings"
)

var referencePattern = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\((/organizations/(org_[A-Za-z0-9_-]+)/api/v2/projects/(prj_[A-Za-z0-9_-]+)/attachments/(att_[a-f0-9]{32}))\)`)

type Reference struct {
	URL   string
	ID    string
	Image bool
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
