package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/visualreview"
)

type visualReviewImportResult struct {
	CaptureID string `json:"capture_id"`
	HeadSHA   string `json:"head_sha"`
	ReviewURL string `json:"review_url"`
}

func newVisualReviewCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "visual-review",
		Short:   "Publish local visual evidence to this Detent node",
		Example: "detent visual-review import --project detent --issue ISSUE_NODE_ID --package /path/to/review-package",
	}
	cmd.AddCommand(newVisualReviewImportCommand(configPath, host, port, opts))
	return cmd
}

func newVisualReviewImportCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var projectID, issueID, packageDir string
	cmd := &cobra.Command{
		Use:     "import",
		Short:   "Import an immutable visual review package through the Detent API",
		Example: "detent visual-review import --project detent --issue ISSUE_NODE_ID --package /path/to/review-package",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := runVisualReviewImport(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), projectID, issueID, packageDir, opts)
			if err != nil {
				return err
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			return out.Write(func(writer io.Writer) error {
				_, err := fmt.Fprintf(writer, "Imported visual review %s for head %s\nReview: %s\n", result.CaptureID, result.HeadSHA, result.ReviewURL)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "project id")
	cmd.Flags().StringVar(&issueID, "issue", "", "issue id")
	cmd.Flags().StringVar(&packageDir, "package", "", "local visual review package directory")
	for _, name := range []string{"project", "issue", "package"} {
		markFlagRequired(cmd, name)
	}
	return cmd
}

func runVisualReviewImport(ctx context.Context, configPath, host string, port int, portSet bool, projectID, issueID, packageDir string, opts options) (visualReviewImportResult, error) {
	packageRoot, err := filepath.EvalSymlinks(packageDir)
	if err != nil {
		return visualReviewImportResult{}, WrapValidation(fmt.Errorf("resolve visual review package: %w", err))
	}
	info, err := os.Stat(packageRoot)
	if err != nil || !info.IsDir() {
		return visualReviewImportResult{}, WrapValidation(errors.New("visual review package must be a directory"))
	}
	manifestPath, err := safeVisualReviewPackageAsset(packageRoot, "manifest.json")
	if err != nil {
		return visualReviewImportResult{}, WrapValidation(err)
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return visualReviewImportResult{}, WrapValidation(fmt.Errorf("read visual review manifest: %w", err))
	}
	manifest, err := visualreview.ValidateManifest(raw)
	if err != nil {
		return visualReviewImportResult{}, WrapValidation(err)
	}

	temp, err := os.CreateTemp("", "detent-visual-review-*.multipart")
	if err != nil {
		return visualReviewImportResult{}, err
	}
	tempPath := temp.Name()
	defer func() {
		closeVisualReviewFile(temp)
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}()
	writer := multipart.NewWriter(temp)
	if err := writer.WriteField("issue_id", strings.TrimSpace(issueID)); err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	part, err := writer.CreateFormFile("manifest", "manifest.json")
	if err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	if _, err := io.Copy(part, bytes.NewReader(raw)); err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	for _, asset := range manifest.Assets {
		assetPath, err := safeVisualReviewPackageAsset(packageRoot, asset.Path)
		if err != nil {
			closeVisualReviewFile(temp)
			return visualReviewImportResult{}, WrapValidation(err)
		}
		assetInfo, err := os.Stat(assetPath)
		if err != nil || !assetInfo.Mode().IsRegular() {
			closeVisualReviewFile(temp)
			return visualReviewImportResult{}, WrapValidation(fmt.Errorf("visual review asset %q must be a regular file", asset.Path))
		}
		if assetInfo.Size() <= 0 || assetInfo.Size() > visualReviewCLIAssetLimit {
			closeVisualReviewFile(temp)
			return visualReviewImportResult{}, WrapValidation(fmt.Errorf("visual review asset %q is empty or exceeds 250 MB", asset.Path))
		}
		file, err := os.Open(assetPath)
		if err != nil {
			closeVisualReviewFile(temp)
			return visualReviewImportResult{}, err
		}
		part, partErr := writer.CreateFormFile("asset:"+asset.ID, filepath.Base(asset.Path))
		if partErr == nil {
			var copied int64
			copied, partErr = io.Copy(part, io.LimitReader(file, visualReviewCLIAssetLimit+1))
			if partErr == nil && copied != assetInfo.Size() {
				partErr = fmt.Errorf("visual review asset %q changed while reading", asset.Path)
			}
		}
		closeErr := file.Close()
		if partErr != nil {
			closeVisualReviewFile(temp)
			return visualReviewImportResult{}, partErr
		}
		if closeErr != nil {
			closeVisualReviewFile(temp)
			return visualReviewImportResult{}, closeErr
		}
	}
	if err := writer.Close(); err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	stat, err := temp.Stat()
	if err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	if stat.Size() > visualReviewCLITotalLimit {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, WrapValidation(errors.New("visual review upload exceeds 512 MB"))
	}

	boot, address, err := resolveDashboardBoot(ctx, configPath, host, port, portSet, opts)
	if err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	token := strings.TrimSpace(opts.lookupEnv("DETENT_API_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(boot.Global.APIToken)
	}
	if token == "" {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, WrapValidation(errors.New("visual review import requires DETENT_API_TOKEN or configured api_token"))
	}
	endpoint := "http://" + dashboardServerAddr(boot) + "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(projectID)) + "/visual-reviews/import"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, temp)
	if err != nil {
		closeVisualReviewFile(temp)
		return visualReviewImportResult{}, err
	}
	request.ContentLength = stat.Size()
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := opts.httpDo(request)
	closeVisualReviewFile(temp)
	if err != nil {
		return visualReviewImportResult{}, fmt.Errorf("import visual review via %s: %w", address, err)
	}
	defer closeVisualReviewBody(response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			return visualReviewImportResult{}, fmt.Errorf("read visual review error response: %w", readErr)
		}
		return visualReviewImportResult{}, fmt.Errorf("import visual review: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result visualReviewImportResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return result, err
	}
	if strings.HasPrefix(result.ReviewURL, "/") {
		result.ReviewURL = "http://" + dashboardServerAddr(boot) + result.ReviewURL
	}
	return result, nil
}

func closeVisualReviewFile(file *os.File) {
	if err := file.Close(); err != nil {
		return
	}
}

func closeVisualReviewBody(body io.Closer) {
	if err := body.Close(); err != nil {
		return
	}
}

const visualReviewCLIAssetLimit = 250 << 20
const visualReviewCLITotalLimit = 512 << 20

func safeVisualReviewPackageAsset(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("visual review asset path must be relative")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(resolvedRoot, filepath.FromSlash(relative))
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("visual review asset %q escapes the package", relative)
	}
	return resolved, nil
}
