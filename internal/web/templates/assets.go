package templates

import "strings"

type AssetPaths struct {
	Stylesheet string
}

func stylesheetPath(assets AssetPaths) string {
	if stylesheet := strings.TrimSpace(assets.Stylesheet); stylesheet != "" {
		return stylesheet
	}
	return "/static/css/output.css"
}
