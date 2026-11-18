package templates

import "strings"

type AssetPaths struct {
	Favicon    string
	Stylesheet string
}

func faviconPath(assets AssetPaths) string {
	if favicon := strings.TrimSpace(assets.Favicon); favicon != "" {
		return favicon
	}
	return "data:,"
}

func stylesheetPath(assets AssetPaths) string {
	if stylesheet := strings.TrimSpace(assets.Stylesheet); stylesheet != "" {
		return stylesheet
	}
	return "/static/css/output.css"
}
