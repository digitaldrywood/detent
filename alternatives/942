package emailtemplate

import (
	"fmt"
	"html/template"
	"strings"
)

type SummaryRow struct {
	Title, Detail, URL string
	Alert              bool
}

type SummaryTile struct {
	Label, Value, Detail string
	Positive             bool
}

type SummaryProject struct {
	Name, Spend, PerIssue, NotShipped string
	Shipped                           int
	CostPerIssue                      float64
	Highlight                         bool
}

type DailySummary struct {
	Organization, Date, Window, Subject                     string
	BoardURL, CostURL, SettingsURL, UnsubscribeURL, MoreURL string
	ShippedCount, More                                      int
	Cost                                                    bool
	Tiles                                                   []SummaryTile
	Needs, Watch, Shipped                                   []SummaryRow
	Projects                                                []SummaryProject
}

func RenderDailySummary(data DailySummary) (Message, error) {
	var html strings.Builder
	if err := dailySummaryLayout.Execute(&html, data); err != nil {
		return Message{}, fmt.Errorf("render daily summary: %w", err)
	}
	message, err := renderMarkup(html.String())
	if err != nil {
		return Message{}, err
	}
	message.Subject = data.Subject
	var body strings.Builder
	fmt.Fprintf(&body, "Daily summary · %s · %s\n%d issues shipped. %d need you.\n%s\n\n", data.Organization, data.Date, data.ShippedCount, len(data.Needs), data.Window)
	if data.Cost {
		for _, tile := range data.Tiles {
			fmt.Fprintf(&body, "%s: %s (%s)\n", tile.Label, tile.Value, tile.Detail)
		}
		body.WriteString("\n")
	}
	for _, section := range []struct {
		name string
		rows []SummaryRow
	}{{"Needs you", data.Needs}, {"Watch", data.Watch}, {"Shipped", data.Shipped}} {
		if len(section.rows) == 0 {
			continue
		}
		fmt.Fprintf(&body, "%s\n", section.name)
		for _, row := range section.rows {
			fmt.Fprintf(&body, "%s\n%s\n%s\n", row.Title, row.Detail, row.URL)
		}
		body.WriteString("\n")
	}
	if data.More > 0 {
		fmt.Fprintf(&body, "and %d more: %s\n\n", data.More, data.MoreURL)
	}
	if data.Cost {
		body.WriteString("Spend by project\nProject | Spend | Shipped | Per issue | Did not ship\n")
		for _, p := range data.Projects {
			fmt.Fprintf(&body, "%s | %s | %d | %s | %s", p.Name, p.Spend, p.Shipped, p.PerIssue, p.NotShipped)
			if p.Highlight {
				body.WriteString(" (above organization 7-day per-issue average)")
			}
			body.WriteString("\n")
		}
		fmt.Fprintf(&body, "\nCost report: %s\n", data.CostURL)
	}
	fmt.Fprintf(&body, "Open board: %s\n\nYou get this because you turned on the daily summary for %s.\nChange time or turn off: %s\nUnsubscribe: %s\n", data.BoardURL, data.Organization, data.SettingsURL, data.UnsubscribeURL)
	message.Body = body.String()
	return message, nil
}

var dailySummaryLayout = template.Must(template.New("daily-summary").Parse(`<!doctype html>
<html lang="en"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light"><meta name="supported-color-schemes" content="light"><title>{{.Subject}}</title>
<style>@media(max-width:480px){.content{padding:24px 20px!important}.header,.footer{padding:20px!important}.headline{font-size:18px!important}.tile{display:block!important;box-sizing:border-box;width:100%!important;padding-right:0!important;margin-bottom:12px}.shipped-detail,.not-shipped{display:none!important}.project-table{font-size:12px!important}}</style></head>
<body bgcolor="#f4f4f5" style="margin:0;padding:0;background:#f4f4f5;color:#27272a;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;font-size:14px;line-height:1.4;color-scheme:light">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td class="outer" align="center" style="padding:32px 16px">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" style="width:100%;max-width:600px;background:#fff;border:1px solid #e4e4e7;border-radius:10px;overflow:hidden">
<tr><td class="header" style="padding:20px 28px;border-bottom:1px solid #e4e4e7"><table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td style="font-size:16px;font-weight:600"><img src="cid:detent-mark" width="24" height="24" alt="" style="vertical-align:middle;margin-right:8px">Detent</td><td align="right" style="font-size:12px;color:#71717b">{{.Organization}}</td></tr></table></td></tr>
<tr><td class="content" style="padding:28px">
<h1 style="margin:0 0 8px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:.6px;text-transform:uppercase">Daily summary · {{.Date}}</h1>
<p class="headline" style="margin:0 0 8px;font-size:20px;font-weight:600">{{.ShippedCount}} issues shipped. {{len .Needs}} need you.</p><p style="margin:0 0 24px;color:#71717b">{{.Window}}</p>
{{if .Cost}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="table-layout:fixed;margin-bottom:24px"><tr>{{range .Tiles}}<td class="tile" width="25%" style="padding-right:8px;vertical-align:middle"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="border:1px solid #e4e4e7;border-radius:8px"><tr><td style="padding:12px"><p style="margin:0 0 4px;color:#71717b;font-size:12px">{{.Label}}</p><p style="margin:0 0 4px;font-size:20px;font-weight:600">{{.Value}}</p><p style="margin:0;color:{{if eq .Label "Did not ship"}}#b45309{{else if .Positive}}#00a66f{{else}}#71717b{{end}};font-size:12px">{{.Detail}}</p></td></tr></table></td>{{end}}</tr></table>{{end}}
{{if .Needs}}<h2 style="margin:0 0 12px;font-size:12px;font-weight:600;letter-spacing:.6px;text-transform:uppercase;color:#b45309">Needs you ({{len .Needs}})</h2>
{{range .Needs}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin-bottom:8px;border:1px solid #e4e4e7;border-left:3px solid {{if .Alert}}#fb2c36{{else}}#fe9a00{{end}};border-radius:8px;background:#fafafa"><tr><td style="padding:12px 14px;overflow-wrap:anywhere"><a href="{{.URL}}" style="color:#27272a;text-decoration:none;font-weight:600">{{.Title}}</a><p style="margin:2px 0 0;color:#71717b;font-size:13px">{{.Detail}}</p></td></tr></table>{{end}}{{end}}
{{if .Watch}}<h2 style="margin:28px 0 12px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:.6px;text-transform:uppercase">Watch ({{len .Watch}})</h2><table role="presentation" width="100%" cellpadding="0" cellspacing="0">{{range .Watch}}<tr><td style="padding:10px 0;border-top:1px solid #f4f4f5;overflow-wrap:anywhere"><a href="{{.URL}}" style="color:#27272a;text-decoration:none;font-weight:500">{{.Title}}</a><p style="margin:2px 0 0;color:#71717b;font-size:13px">{{.Detail}}</p></td></tr>{{end}}</table>{{end}}
{{if .Shipped}}<h2 style="margin:28px 0 12px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:.6px;text-transform:uppercase">Shipped ({{.ShippedCount}})</h2><table role="presentation" width="100%" cellpadding="0" cellspacing="0">{{range .Shipped}}<tr><td style="padding:10px 0;border-top:1px solid #f4f4f5;overflow-wrap:anywhere"><a href="{{.URL}}" style="color:#27272a;text-decoration:none">{{.Title}}</a><span class="shipped-detail" style="float:right;color:#71717b;font-size:13px;text-align:right">{{.Detail}}</span></td></tr>{{end}}</table>{{end}}
{{if .More}}<p style="margin:8px 0 0"><a href="{{.MoreURL}}" style="color:#346bf1;text-decoration:none">and {{.More}} more →</a></p>{{end}}
{{if .Cost}}<h2 style="margin:28px 0 12px;color:#71717b;font-size:12px;font-weight:600;letter-spacing:.6px;text-transform:uppercase">Spend by project</h2><table class="project-table" aria-label="Spend by project" width="100%" cellpadding="0" cellspacing="0" style="font-size:13px;border-collapse:collapse"><tr style="color:#71717b"><th align="left" style="padding:0 4px 8px 0;font-weight:400">Project</th><th align="right" style="font-weight:400">Spend</th><th align="right" style="font-weight:400">Shipped</th><th align="right" style="font-weight:400">Per issue</th><th class="not-shipped" align="right" style="font-weight:400">Did not ship</th></tr>{{range .Projects}}<tr><td style="padding:9px 4px 9px 0;border-top:1px solid #f4f4f5;overflow-wrap:anywhere">{{.Name}}</td><td align="right" style="border-top:1px solid #f4f4f5">{{.Spend}}</td><td align="right" style="border-top:1px solid #f4f4f5">{{.Shipped}}</td><td align="right" style="border-top:1px solid #f4f4f5;color:{{if .Highlight}}#b45309{{else}}#27272a{{end}}">{{.PerIssue}}</td><td class="not-shipped" align="right" style="border-top:1px solid #f4f4f5">{{.NotShipped}}</td></tr>{{end}}</table>{{end}}
<table role="presentation" cellpadding="0" cellspacing="0" style="margin-top:28px"><tr><td style="background:#27272a;border-radius:8px"><a href="{{.BoardURL}}" style="display:inline-block;padding:12px 16px;color:#fff;font-weight:600;text-decoration:none">Open board</a></td>{{if .Cost}}<td style="padding-left:12px"><a href="{{.CostURL}}" style="color:#346bf1;text-decoration:none">Cost report →</a></td>{{end}}</tr></table>
</td></tr><tr><td class="footer" style="padding:16px 28px;border-top:1px solid #e4e4e7;color:#71717b;font-size:12px">You get this because you turned on the daily summary for {{.Organization}}. <a href="{{.SettingsURL}}" style="color:#71717b">Change time or turn off</a> · <a href="{{.UnsubscribeURL}}" style="color:#71717b">Unsubscribe</a></td></tr>
</table></td></tr></table></body></html>`))
