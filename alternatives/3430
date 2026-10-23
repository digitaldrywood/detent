package templates

import "strings"

type AssetPaths struct {
	Favicon         string
	Stylesheet      string
	ChartJS         string
	DashboardCharts string
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

func chartJSPath(assets AssetPaths) string {
	if script := strings.TrimSpace(assets.ChartJS); script != "" {
		return script
	}
	return "/static/vendor/chartjs/chart.umd.min.js"
}

func dashboardChartsScriptPath(assets AssetPaths) string {
	if script := strings.TrimSpace(assets.DashboardCharts); script != "" {
		return script
	}
	return "/static/js/dashboard-charts.js"
}
