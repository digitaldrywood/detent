package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/web/templates"
	"github.com/digitaldrywood/detent/internal/workitem"
)

func newReportCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var htmlPath string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Export the operations report from the running Detent service",
		Long:  "Read the operations report (stats, actions taken, decisions needed) from the running Detent service and write it as a self-contained HTML file. The file states its data time and producing instance and is written atomically so readers never see a partial page.",
		Example: strings.TrimSpace(`detent report --html ~/Dropbox/Detent/Detent\ Status.html
detent report --html status.html --format json`),
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(htmlPath) == "" {
				return NewValidationError("--html is required", "Pass the file path to write, for example --html status.html.", nil)
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			client, err := newDashboardReadClient(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), opts)
			if err != nil {
				return err
			}
			result, err := runReportHTML(cmd.Context(), client, htmlPath)
			if err != nil {
				return classifyReportReadError(err)
			}
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "Wrote %s (%s, data time %s, %d bytes)\n", result.Path, result.Instance, result.DataTime, result.Bytes)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&htmlPath, "html", "", "write the operations report to this HTML file")
	return cmd
}

type reportHTMLResult struct {
	Path     string `json:"path"`
	Instance string `json:"instance"`
	DataTime string `json:"data_time"`
	Bytes    int    `json:"bytes"`
}

func runReportHTML(ctx context.Context, client *DashboardReadClient, path string) (reportHTMLResult, error) {
	report, err := client.Operations(ctx)
	if err != nil {
		return reportHTMLResult{}, err
	}
	absolutizeReportLinks(&report, client.baseURL)
	var body bytes.Buffer
	if err := templates.OperationsDocument(report).Render(ctx, &body); err != nil {
		return reportHTMLResult{}, fmt.Errorf("render operations report: %w", err)
	}
	if err := writeFileAtomically(path, body.Bytes()); err != nil {
		return reportHTMLResult{}, fmt.Errorf("write operations report: %w", err)
	}
	return reportHTMLResult{Path: path, Instance: report.Instance, DataTime: report.DataTime.UTC().Format("2006-01-02T15:04:05Z07:00"), Bytes: body.Len()}, nil
}

func (c *DashboardReadClient) Operations(ctx context.Context) (operations.Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return operations.Report{}, err
	}
	if c == nil || c.baseURL == nil {
		return operations.Report{}, errors.New("dashboard API client is not configured")
	}
	requestURL := *c.baseURL
	requestURL.Path = "/api/v1/operations"
	requestURL.RawPath = ""
	var report operations.Report
	if _, err := c.requestJSON(ctx, http.MethodGet, requestURL, &report); err != nil {
		return operations.Report{}, err
	}
	return report, nil
}

// The exported page is read away from the dashboard, so service-relative
// evidence links must carry the service origin to stay clickable. API
// explanation links need a bearer token a browser never sends, so they
// become the issue's dashboard page, which a web session can open.
func absolutizeReportLinks(report *operations.Report, base *url.URL) {
	if base == nil {
		return
	}
	for i := range report.Actions {
		report.Actions[i].EvidenceURL = absolutizeReportLink(report.Actions[i].EvidenceURL, base)
	}
	for i := range report.Decisions {
		report.Decisions[i].URL = absolutizeReportLink(report.Decisions[i].URL, base)
		if report.Decisions[i].Prerequisite != nil {
			report.Decisions[i].Prerequisite.URL = absolutizeReportLink(report.Decisions[i].Prerequisite.URL, base)
		}
	}
}

func absolutizeReportLink(link string, base *url.URL) string {
	if !strings.HasPrefix(link, "/") {
		return link
	}
	parsed, err := url.Parse(link)
	if err != nil {
		return link
	}
	if projectID, ok := strings.CutPrefix(parsed.Path, "/api/v1/projects/"); ok && strings.HasSuffix(projectID, "/issues/explanation") {
		projectID = strings.TrimSuffix(projectID, "/issues/explanation")
		if reference := strings.TrimSpace(parsed.Query().Get("reference")); projectID != "" && reference != "" {
			return workitem.WorkItemURL(base.String(), projectID, reference)
		}
	}
	return base.ResolveReference(parsed).String()
}

func classifyReportReadError(err error) error {
	var response *DashboardResponseError
	if errors.As(err, &response) && response.StatusCode == http.StatusNotFound {
		return NewClassifiedError(ErrDashboardUnsupportedModel, errorCodeUnsupportedModelVersion, "running service does not serve the operations report API", "Upgrade or restart Detent so the running service provides /api/v1/operations.", nil)
	}
	return classifyDashboardReadError(err)
}

func writeFileAtomically(path string, content []byte) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".detent-report-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath == "" {
			return
		}
		if cleanupErr := os.Remove(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(content); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	temporaryPath = ""
	return nil
}
