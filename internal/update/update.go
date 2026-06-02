package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
)

const (
	defaultAPIBase          = "https://api.github.com/repos/digitaldrywood/detent"
	moduleInstallCommand    = "go install github.com/digitaldrywood/detent/cmd/detent@latest"
	homebrewUpdateCommand   = "brew upgrade digitaldrywood/tap/detent"
	defaultChecksumName     = "checksums.txt"
	projectName             = "detent"
	windowsExecutableName   = "detent.exe"
	nonWindowsArchiveExt    = ".tar.gz"
	windowsArchiveExt       = ".zip"
	defaultRequestUserAgent = "detent-updater"
)

var (
	ErrConfirmationRequired = errors.New("update confirmation required")
	ErrRefused              = errors.New("update refused")
)

type InstallSource string

const (
	InstallSourceRelease     InstallSource = "release"
	InstallSourceHomebrew    InstallSource = "homebrew"
	InstallSourceGoInstall   InstallSource = "go_install"
	InstallSourceDevelopment InstallSource = "development"
	InstallSourceUnknown     InstallSource = "unknown"
)

type Action string

const (
	ActionAvailable Action = "available"
	ActionDelegate  Action = "delegate"
	ActionRefused   Action = "refused"
	ActionUpToDate  Action = "up_to_date"
	ActionUpdated   Action = "updated"
)

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type Release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

type ReleaseAssets struct {
	Archive  Asset
	Checksum Asset
}

type ReleaseClient interface {
	ListReleases(context.Context) ([]Release, error)
	Download(context.Context, string) ([]byte, error)
}

type GitHubClientConfig struct {
	APIBase    string
	HTTPClient *http.Client
}

type GitHubClient struct {
	apiBase string
	http    *http.Client
}

func NewGitHubClient(cfg GitHubClientConfig) *GitHubClient {
	apiBase := strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &GitHubClient{apiBase: apiBase, http: httpClient}
}

func (c *GitHubClient) ListReleases(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/releases?per_page=20", nil)
	if err != nil {
		return nil, fmt.Errorf("create releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", defaultRequestUserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("list releases: GitHub returned %s", resp.Status)
	}

	var releases []Release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return releases, nil
}

func (c *GitHubClient) Download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", defaultRequestUserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("download %s: server returned %s", url, resp.Status)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read download %s: %w", url, err)
	}
	return raw, nil
}

type DetectionOptions struct {
	CurrentVersion string
	ExecutablePath string
	GOOS           string
	HomeDir        string
	Env            map[string]string
	EvalSymlinks   func(string) (string, error)
}

type InstallInfo struct {
	Source  InstallSource `json:"source"`
	Command string        `json:"command,omitempty"`
	Message string        `json:"message,omitempty"`
	Binary  string        `json:"binary,omitempty"`
}

func DetectInstallSource(opts DetectionOptions) InstallInfo {
	goos := firstNonEmpty(opts.GOOS, runtime.GOOS)
	executable := cleanPath(opts.ExecutablePath, goos)
	realExecutable := executable
	evalSymlinks := opts.EvalSymlinks
	if evalSymlinks == nil {
		evalSymlinks = filepath.EvalSymlinks
	}
	if real, err := evalSymlinks(opts.ExecutablePath); err == nil && strings.TrimSpace(real) != "" {
		realExecutable = cleanPath(real, goos)
	}

	if isHomebrewPath(executable) || isHomebrewPath(realExecutable) {
		return InstallInfo{
			Source:  InstallSourceHomebrew,
			Command: homebrewUpdateCommand,
			Binary:  opts.ExecutablePath,
		}
	}
	if releaseLockMatches(executable, realExecutable, goos, opts) || windowsInstallerPathMatches(executable, goos, opts) {
		return InstallInfo{
			Source: InstallSourceRelease,
			Binary: opts.ExecutablePath,
		}
	}
	if isGoInstallPath(executable, goos, opts.HomeDir, opts.Env) || isGoInstallPath(realExecutable, goos, opts.HomeDir, opts.Env) {
		return InstallInfo{
			Source:  InstallSourceGoInstall,
			Command: moduleInstallCommand,
			Binary:  opts.ExecutablePath,
		}
	}
	if IsDevelopmentVersion(opts.CurrentVersion) {
		return InstallInfo{
			Source: InstallSourceDevelopment,
			Binary: opts.ExecutablePath,
		}
	}
	return InstallInfo{
		Source: InstallSourceUnknown,
		Binary: opts.ExecutablePath,
	}
}

func IsDevelopmentVersion(version string) bool {
	trimmed := strings.TrimSpace(version)
	switch trimmed {
	case "", "dev", "none", "unknown", "(devel)":
		return true
	}
	_, err := parseVersion(trimmed)
	return err != nil
}

type Config struct {
	CurrentVersion string
	ExecutablePath string
	GOOS           string
	GOARCH         string
	Client         ReleaseClient
	HomeDir        string
	Env            map[string]string
	EvalSymlinks   func(string) (string, error)
}

type Service struct {
	cfg Config
}

type ApplyOptions struct {
	AssumeYes bool
	Confirm   func(Status) (bool, error)
}

type Status struct {
	CurrentVersion  string        `json:"current_version"`
	LatestVersion   string        `json:"latest_version,omitempty"`
	LatestTag       string        `json:"latest_tag,omitempty"`
	UpdateAvailable bool          `json:"update_available"`
	InstallSource   InstallSource `json:"install_source"`
	Action          Action        `json:"action"`
	Message         string        `json:"message,omitempty"`
	Command         string        `json:"command,omitempty"`
	Binary          string        `json:"binary,omitempty"`
	Asset           string        `json:"asset,omitempty"`
}

func NewService(cfg Config) *Service {
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.Client == nil {
		cfg.Client = NewGitHubClient(GitHubClientConfig{})
	}
	return &Service{cfg: cfg}
}

func (s *Service) Check(ctx context.Context) (Status, error) {
	status, _, err := s.plan(ctx)
	return status, err
}

func (s *Service) Apply(ctx context.Context, opts ApplyOptions) (Status, error) {
	status, release, err := s.plan(ctx)
	if err != nil {
		return status, err
	}
	if !status.UpdateAvailable {
		status.Action = ActionUpToDate
		status.Message = fmt.Sprintf("Detent %s is up to date.", status.CurrentVersion)
		return status, nil
	}

	switch status.InstallSource {
	case InstallSourceHomebrew:
		status.Action = ActionDelegate
		status.Command = homebrewUpdateCommand
		status.Message = "Homebrew-managed Detent install detected. Run the Homebrew upgrade command."
		return status, nil
	case InstallSourceRelease:
	case InstallSourceGoInstall:
		status.Action = ActionRefused
		status.Command = moduleInstallCommand
		status.Message = "This Detent binary appears to be managed by go install. Run the Go install command instead."
		return status, ErrRefused
	case InstallSourceDevelopment:
		status.Action = ActionRefused
		status.Message = "This Detent binary does not include release version metadata. Install a published release before using self-update."
		return status, ErrRefused
	default:
		status.Action = ActionRefused
		status.Message = "Detent cannot verify that this binary was installed by the release installer, so it will not overwrite it."
		return status, ErrRefused
	}

	if !opts.AssumeYes {
		if opts.Confirm == nil {
			status.Action = ActionRefused
			status.Message = "Update requires confirmation. Rerun with --yes to update non-interactively."
			return status, ErrConfirmationRequired
		}
		confirmed, err := opts.Confirm(status)
		if err != nil {
			status.Action = ActionRefused
			status.Message = err.Error()
			return status, err
		}
		if !confirmed {
			status.Action = ActionRefused
			status.Message = "Update cancelled."
			return status, ErrConfirmationRequired
		}
	}

	assets, err := SelectReleaseAssets(release, s.cfg.GOOS, s.cfg.GOARCH)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}
	archive, err := s.cfg.Client.Download(ctx, assets.Archive.BrowserDownloadURL)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}
	checksums, err := s.cfg.Client.Download(ctx, assets.Checksum.BrowserDownloadURL)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}
	if err := VerifyChecksum(checksums, assets.Archive.Name, archive); err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}

	binary, mode, err := ExtractBinary(archive, s.cfg.GOOS)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}
	if err := ReplaceBinary(s.cfg.ExecutablePath, binary, mode); err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}

	status.Action = ActionUpdated
	status.Asset = assets.Archive.Name
	status.Message = fmt.Sprintf("Updated Detent from %s to %s.", status.CurrentVersion, status.LatestVersion)
	return status, nil
}

func (s *Service) plan(ctx context.Context) (Status, Release, error) {
	info := DetectInstallSource(DetectionOptions{
		CurrentVersion: s.cfg.CurrentVersion,
		ExecutablePath: s.cfg.ExecutablePath,
		GOOS:           s.cfg.GOOS,
		HomeDir:        s.cfg.HomeDir,
		Env:            s.cfg.Env,
		EvalSymlinks:   s.cfg.EvalSymlinks,
	})
	status := Status{
		CurrentVersion: strings.TrimSpace(s.cfg.CurrentVersion),
		InstallSource:  info.Source,
		Command:        info.Command,
		Binary:         s.cfg.ExecutablePath,
	}

	if IsDevelopmentVersion(s.cfg.CurrentVersion) {
		status.Action = ActionRefused
		switch info.Source {
		case InstallSourceGoInstall:
			status.Message = "This Detent binary appears to be managed by go install and does not include release metadata. Run the Go install command instead."
			status.Command = moduleInstallCommand
		default:
			status.Message = "This Detent binary does not include release version metadata. Install a published release before using self-update."
		}
		return status, Release{}, ErrRefused
	}

	releases, err := s.cfg.Client.ListReleases(ctx)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, Release{}, err
	}
	release, ok, err := SelectLatestRelease(s.cfg.CurrentVersion, releases)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, Release{}, err
	}
	if !ok {
		status.Action = ActionRefused
		status.Message = "No eligible Detent release was found."
		return status, Release{}, errors.New("no eligible Detent release found")
	}

	status.LatestTag = release.TagName
	status.LatestVersion = displayVersion(release.TagName)
	cmp, err := CompareVersions(release.TagName, s.cfg.CurrentVersion)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, Release{}, err
	}
	status.UpdateAvailable = cmp > 0
	if status.UpdateAvailable {
		status.Action = ActionAvailable
		status.Message = fmt.Sprintf("Detent %s can be updated to %s.", status.CurrentVersion, status.LatestVersion)
	} else {
		status.Action = ActionUpToDate
		status.Message = fmt.Sprintf("Detent %s is up to date.", status.CurrentVersion)
	}
	return status, release, nil
}

func SelectLatestRelease(current string, releases []Release) (Release, bool, error) {
	currentVersion, err := parseVersion(current)
	if err != nil {
		return Release{}, false, err
	}

	var latest Release
	var latestVersion semVersion
	found := false
	for _, release := range releases {
		if release.Draft {
			continue
		}
		if release.Prerelease && len(currentVersion.prerelease) == 0 {
			continue
		}
		version, err := parseVersion(release.TagName)
		if err != nil {
			continue
		}
		if !found || compareSemVersions(version, latestVersion) > 0 {
			latest = release
			latestVersion = version
			found = true
		}
	}
	return latest, found, nil
}

func CompareVersions(a string, b string) (int, error) {
	versionA, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	versionB, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return compareSemVersions(versionA, versionB), nil
}

func SelectReleaseAssets(release Release, goos string, goarch string) (ReleaseAssets, error) {
	archiveNames := archiveAssetNames(release.TagName, goos, goarch)
	checksumNames := checksumAssetNames(release.TagName)

	archive, ok := assetByName(release.Assets, archiveNames)
	if !ok {
		return ReleaseAssets{}, fmt.Errorf("release %s does not include an archive for %s/%s", release.TagName, goos, goarch)
	}
	checksum, ok := assetByName(release.Assets, checksumNames)
	if !ok {
		return ReleaseAssets{}, fmt.Errorf("release %s does not include a checksum asset", release.TagName)
	}
	return ReleaseAssets{Archive: archive, Checksum: checksum}, nil
}

func VerifyChecksum(checksums []byte, assetName string, archive []byte) error {
	expected, ok := expectedChecksum(checksums, assetName)
	if !ok {
		return fmt.Errorf("checksum for %s not found", assetName)
	}
	sum := sha256.Sum256(archive)
	actual := fmt.Sprintf("%x", sum)
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, expected, actual)
	}
	return nil
}

func ExtractBinary(archive []byte, goos string) ([]byte, os.FileMode, error) {
	if goos == "windows" {
		return extractZipBinary(archive)
	}
	return extractTarGzipBinary(archive)
}

func ReplaceBinary(target string, binary []byte, mode os.FileMode) error {
	if strings.TrimSpace(target) == "" {
		return errors.New("target binary path is required")
	}
	if mode == 0 {
		mode = 0o755
	}

	dir := filepath.Dir(target)
	base := filepath.Base(target)
	temp, err := os.CreateTemp(dir, "."+base+".update-*")
	if err != nil {
		return fmt.Errorf("create update temp file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			removeFile(tempPath)
		}
	}()

	writeErr := writeBinaryTemp(temp, binary, mode)
	if writeErr != nil {
		return writeErr
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("replace binary %s: %w", target, err)
	}
	cleanup = false
	return nil
}

func writeBinaryTemp(file *os.File, binary []byte, mode os.FileMode) error {
	if _, err := file.Write(binary); err != nil {
		closeErr := file.Close()
		if closeErr != nil {
			return fmt.Errorf("write update temp file: %w; close temp file: %w", err, closeErr)
		}
		return fmt.Errorf("write update temp file: %w", err)
	}
	if err := file.Chmod(mode); err != nil {
		closeErr := file.Close()
		if closeErr != nil {
			return fmt.Errorf("chmod update temp file: %w; close temp file: %w", err, closeErr)
		}
		return fmt.Errorf("chmod update temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close update temp file: %w", err)
	}
	return nil
}

func removeFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

func extractTarGzipBinary(archive []byte) (_ []byte, _ os.FileMode, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, 0, fmt.Errorf("open release archive: %w", err)
	}
	defer func() {
		if closeErr := gz.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("close release archive: %w", closeErr)
		}
	}()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read release archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			continue
		}
		if filepath.Base(header.Name) != projectName {
			continue
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			return nil, 0, fmt.Errorf("read detent from release archive: %w", err)
		}
		return raw, os.FileMode(header.Mode), nil
	}
	return nil, 0, errors.New("release archive did not contain detent")
}

func extractZipBinary(archive []byte) ([]byte, os.FileMode, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, 0, fmt.Errorf("open release archive: %w", err)
	}
	for _, file := range zr.File {
		if filepath.Base(file.Name) != windowsExecutableName {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return nil, 0, fmt.Errorf("open detent.exe from release archive: %w", err)
		}
		raw, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, 0, fmt.Errorf("read detent.exe from release archive: %w", readErr)
		}
		if closeErr != nil {
			return nil, 0, fmt.Errorf("close detent.exe from release archive: %w", closeErr)
		}
		mode := file.Mode()
		if mode == 0 {
			mode = 0o755
		}
		return raw, mode, nil
	}
	return nil, 0, errors.New("release archive did not contain detent.exe")
}

func expectedChecksum(checksums []byte, assetName string) (string, bool) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == assetName {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func archiveAssetNames(tag string, goos string, goarch string) []string {
	version := displayVersion(tag)
	extension := nonWindowsArchiveExt
	if goos == "windows" {
		extension = windowsArchiveExt
	}
	names := []string{
		fmt.Sprintf("%s_%s_%s_%s%s", projectName, version, goos, goarch, extension),
	}
	if tag != version {
		names = append(names, fmt.Sprintf("%s_%s_%s_%s%s", projectName, tag, goos, goarch, extension))
	}
	return names
}

func checksumAssetNames(tag string) []string {
	version := displayVersion(tag)
	names := []string{
		fmt.Sprintf("%s_%s_checksums.txt", projectName, version),
		defaultChecksumName,
	}
	if tag != version {
		names = append([]string{fmt.Sprintf("%s_%s_checksums.txt", projectName, tag)}, names...)
	}
	return names
}

func assetByName(assets []Asset, names []string) (Asset, bool) {
	for _, name := range names {
		for _, asset := range assets {
			if asset.Name == name {
				return asset, true
			}
		}
	}
	return Asset{}, false
}

func displayVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

type semVersion struct {
	major      int
	minor      int
	patch      int
	prerelease []string
}

func parseVersion(version string) (semVersion, error) {
	trimmed := displayVersion(version)
	withoutBuild, _, _ := strings.Cut(trimmed, "+")
	core, prerelease, hasPrerelease := strings.Cut(withoutBuild, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, fmt.Errorf("invalid semantic version: %s", version)
	}

	major, err := parseVersionNumber(parts[0], version)
	if err != nil {
		return semVersion{}, err
	}
	minor, err := parseVersionNumber(parts[1], version)
	if err != nil {
		return semVersion{}, err
	}
	patch, err := parseVersionNumber(parts[2], version)
	if err != nil {
		return semVersion{}, err
	}

	var prereleaseParts []string
	if hasPrerelease {
		prereleaseParts = strings.Split(prerelease, ".")
		for _, part := range prereleaseParts {
			if part == "" {
				return semVersion{}, fmt.Errorf("invalid semantic version: %s", version)
			}
		}
	}
	return semVersion{major: major, minor: minor, patch: patch, prerelease: prereleaseParts}, nil
}

func parseVersionNumber(part string, original string) (int, error) {
	if part == "" {
		return 0, fmt.Errorf("invalid semantic version: %s", original)
	}
	for _, r := range part {
		if !unicode.IsDigit(r) {
			return 0, fmt.Errorf("invalid semantic version: %s", original)
		}
	}
	value, err := strconv.Atoi(part)
	if err != nil {
		return 0, fmt.Errorf("invalid semantic version: %s", original)
	}
	return value, nil
}

func compareSemVersions(a semVersion, b semVersion) int {
	for _, values := range [][2]int{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if values[0] > values[1] {
			return 1
		}
		if values[0] < values[1] {
			return -1
		}
	}
	return comparePrerelease(a.prerelease, b.prerelease)
}

func comparePrerelease(a []string, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		cmp := comparePrereleaseIdentifier(a[i], b[i])
		if cmp != 0 {
			return cmp
		}
	}
	if len(a) > len(b) {
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	return 0
}

func comparePrereleaseIdentifier(a string, b string) int {
	aNumeric := isNumericIdentifier(a)
	bNumeric := isNumericIdentifier(b)
	switch {
	case aNumeric && bNumeric:
		return compareNumericStrings(a, b)
	case aNumeric:
		return -1
	case bNumeric:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func isNumericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func compareNumericStrings(a string, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	if len(a) > len(b) {
		return 1
	}
	if len(a) < len(b) {
		return -1
	}
	return strings.Compare(a, b)
}

func releaseLockMatches(executable string, realExecutable string, goos string, opts DetectionOptions) bool {
	for _, candidate := range installLockCandidates(goos, opts) {
		binary, ok := readInstallLockBinary(candidate)
		if !ok {
			continue
		}
		cleanBinary := cleanPath(binary, goos)
		if samePath(cleanBinary, executable, goos) || samePath(cleanBinary, realExecutable, goos) {
			return true
		}
	}
	return false
}

func installLockCandidates(goos string, opts DetectionOptions) []string {
	var candidates []string
	if lockPath := envValue(opts.Env, "DETENT_INSTALL_LOCK"); lockPath != "" {
		candidates = append(candidates, lockPath)
	}
	if stateDir := envValue(opts.Env, "DETENT_STATE_DIR"); stateDir != "" {
		candidates = append(candidates, filepath.Join(stateDir, "install.lock"))
	}
	home := homeDir(opts.HomeDir, opts.Env)
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".detent", "install.lock"))
	}
	if goos == "windows" {
		if localAppData := envValue(opts.Env, "LOCALAPPDATA"); localAppData != "" {
			candidates = append(candidates, filepath.Join(localAppData, "detent", "install.lock"))
		}
	}
	return candidates
}

func readInstallLockBinary(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "binary" {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func windowsInstallerPathMatches(executable string, goos string, opts DetectionOptions) bool {
	if goos != "windows" {
		return false
	}
	for _, installDir := range windowsInstallDirs(opts) {
		if samePath(executable, filepath.Join(installDir, windowsExecutableName), goos) {
			return true
		}
	}
	return false
}

func windowsInstallDirs(opts DetectionOptions) []string {
	var dirs []string
	if installDir := envValue(opts.Env, "DETENT_INSTALL_DIR"); installDir != "" {
		dirs = append(dirs, installDir)
	}
	if localAppData := envValue(opts.Env, "LOCALAPPDATA"); localAppData != "" {
		dirs = append(dirs, filepath.Join(localAppData, "detent", "bin"))
	}
	if home := homeDir(opts.HomeDir, opts.Env); home != "" {
		dirs = append(dirs, filepath.Join(home, ".detent", "bin"))
	}
	return dirs
}

func isHomebrewPath(path string) bool {
	normalized := filepath.ToSlash(path)
	return strings.Contains(normalized, "/Cellar/detent/")
}

func isGoInstallPath(path string, goos string, explicitHome string, env map[string]string) bool {
	executableName := projectName
	if goos == "windows" {
		executableName = windowsExecutableName
	}
	if gobin := envValue(env, "GOBIN"); gobin != "" && samePath(path, filepath.Join(gobin, executableName), goos) {
		return true
	}
	gopath := envValue(env, "GOPATH")
	if gopath == "" {
		if home := homeDir(explicitHome, env); home != "" {
			gopath = filepath.Join(home, "go")
		}
	}
	for _, root := range filepath.SplitList(gopath) {
		if root == "" {
			continue
		}
		if samePath(path, filepath.Join(root, "bin", executableName), goos) {
			return true
		}
	}
	return false
}

func homeDir(explicitHome string, env map[string]string) string {
	if explicitHome != "" {
		return explicitHome
	}
	if home := envValue(env, "HOME"); home != "" {
		return home
	}
	if home := envValue(env, "USERPROFILE"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func envValue(env map[string]string, key string) string {
	if env == nil {
		return os.Getenv(key)
	}
	for existingKey, value := range env {
		if strings.EqualFold(existingKey, key) {
			return value
		}
	}
	return ""
}

func samePath(a string, b string, goos string) bool {
	a = cleanPath(a, goos)
	b = cleanPath(b, goos)
	if goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func cleanPath(path string, goos string) string {
	if path == "" {
		return ""
	}
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		cleaned = abs
	}
	if goos == "windows" {
		cleaned = strings.TrimRight(cleaned, `\/`)
	}
	return cleaned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
