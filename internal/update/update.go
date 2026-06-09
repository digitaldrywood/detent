package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/blake2b"
)

const (
	defaultAPIBase           = "https://api.github.com/repos/digitaldrywood/detent"
	moduleInstallPackage     = "github.com/digitaldrywood/detent/cmd/detent"
	moduleInstallTarget      = moduleInstallPackage + "@latest"
	moduleInstallCommand     = "go install " + moduleInstallTarget
	homebrewUpdateCommand    = "brew upgrade digitaldrywood/tap/detent"
	defaultChecksumName      = "checksums.txt"
	projectName              = "detent"
	windowsExecutableName    = "detent.exe"
	nonWindowsArchiveExt     = ".tar.gz"
	windowsArchiveExt        = ".zip"
	defaultRequestUserAgent  = "detent-updater"
	defaultHTTPClientTimeout = 2 * time.Minute
	defaultMaxDownloadBytes  = 256 * 1024 * 1024
	minisignPublicKeySize    = 42
	minisignSignatureSize    = 74
)

var (
	ErrConfirmationRequired = errors.New("update confirmation required")
	ErrRefused              = errors.New("update refused")

	defaultChecksumMinisignPublicKey = "RWR9LKG/4dIAmbeS4Ow7XOSjxOE7GKiwQ0OPHNoViW5V0FSo4WU0vucx"
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
	Archive           Asset
	Checksum          Asset
	ChecksumSignature Asset
}

type ReleaseClient interface {
	ListReleases(context.Context) ([]Release, error)
	Download(context.Context, string) ([]byte, error)
}

type ProcessStarter func(string, []string) error
type CommandRunner func(context.Context, string, []string, io.Writer, io.Writer) error
type BinaryVerifier func(context.Context, string) (string, error)
type BinarySigner func(context.Context, string) error
type CodeSignatureVerifier func(context.Context, string) (bool, error)
type ChecksumSignatureVerifier func(context.Context, []byte, []byte) error

type GitHubClientConfig struct {
	APIBase          string
	HTTPClient       *http.Client
	MaxDownloadBytes int64
}

type GitHubClient struct {
	apiBase          string
	http             *http.Client
	maxDownloadBytes int64
}

func NewGitHubClient(cfg GitHubClientConfig) *GitHubClient {
	apiBase := strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPClientTimeout}
	}
	maxDownloadBytes := cfg.MaxDownloadBytes
	if maxDownloadBytes <= 0 {
		maxDownloadBytes = defaultMaxDownloadBytes
	}
	return &GitHubClient{apiBase: apiBase, http: httpClient, maxDownloadBytes: maxDownloadBytes}
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

	if resp.ContentLength > c.maxDownloadBytes {
		return nil, fmt.Errorf("download %s exceeds maximum download size of %d bytes", url, c.maxDownloadBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read download %s: %w", url, err)
	}
	if int64(len(raw)) > c.maxDownloadBytes {
		return nil, fmt.Errorf("download %s exceeds maximum download size of %d bytes", url, c.maxDownloadBytes)
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
	CurrentVersion            string
	ExecutablePath            string
	GOOS                      string
	GOARCH                    string
	Client                    ReleaseClient
	CommandRunner             CommandRunner
	BinaryVerifier            BinaryVerifier
	BinarySigner              BinarySigner
	ChecksumSignatureVerifier ChecksumSignatureVerifier
	RequireChecksumSignature  bool
	HomeDir                   string
	Env                       map[string]string
	EvalSymlinks              func(string) (string, error)
}

type Service struct {
	cfg Config
}

type ApplyOptions struct {
	AssumeYes             bool
	FromRelease           bool
	Confirm               func(Status) (bool, error)
	SelectGoInstallAction func(Status) (GoInstallAction, error)
	Stdout                io.Writer
	Stderr                io.Writer
}

type GoInstallAction string

const (
	GoInstallActionRun     GoInstallAction = "run_go_install"
	GoInstallActionRelease GoInstallAction = "switch_to_release"
	GoInstallActionAbort   GoInstallAction = "abort"
)

type Replacement struct {
	Target                string
	Binary                []byte
	Mode                  os.FileMode
	GOOS                  string
	Context               context.Context
	StartProcess          ProcessStarter
	Verify                BinaryVerifier
	Sign                  BinarySigner
	CodeSignatureVerifier CodeSignatureVerifier
	AfterReplace          func(context.Context, string) error
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
	checksumSignatureVerifierConfigured := cfg.ChecksumSignatureVerifier != nil
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.Client == nil {
		cfg.Client = NewGitHubClient(GitHubClientConfig{})
	}
	if cfg.CommandRunner == nil {
		cfg.CommandRunner = runCommand
	}
	if cfg.BinaryVerifier == nil {
		cfg.BinaryVerifier = verifyBinaryVersion
	}
	if cfg.BinarySigner == nil {
		cfg.BinarySigner = signBinary
	}
	if cfg.ChecksumSignatureVerifier == nil {
		cfg.ChecksumSignatureVerifier = VerifyChecksumSignature
	}
	if !cfg.RequireChecksumSignature {
		cfg.RequireChecksumSignature = checksumSignatureVerifierConfigured || checksumSignaturePublicKeyConfigured()
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
		status.Command = moduleInstallCommand
		return s.applyGoInstallUpdate(ctx, status, release, opts)
	case InstallSourceDevelopment:
		status.Action = ActionRefused
		status.Message = "This Detent binary does not include release version metadata. Install a published release before using self-update."
		return status, ErrRefused
	default:
		status.Action = ActionRefused
		status.Message = "Detent cannot verify that this binary was installed by the release installer, so it will not overwrite it."
		return status, ErrRefused
	}

	if !opts.AssumeYes && !opts.FromRelease {
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

	return s.applyReleaseUpdate(ctx, status, release, opts, false, false)
}

func (s *Service) applyGoInstallUpdate(ctx context.Context, status Status, release Release, opts ApplyOptions) (Status, error) {
	status.Command = goInstallCommand(status)
	action, err := goInstallAction(status, opts)
	if err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}

	switch action {
	case GoInstallActionRun:
		return s.runGoInstallUpdate(ctx, status, opts)
	case GoInstallActionRelease:
		return s.applyReleaseUpdate(ctx, status, release, opts, true, opts.FromRelease)
	case GoInstallActionAbort:
		status.Action = ActionRefused
		status.Message = "Update aborted."
		return status, ErrRefused
	default:
		status.Action = ActionRefused
		status.Message = fmt.Sprintf("unsupported go install update action: %s", action)
		return status, ErrRefused
	}
}

func goInstallAction(status Status, opts ApplyOptions) (GoInstallAction, error) {
	if opts.FromRelease {
		return GoInstallActionRelease, nil
	}
	if opts.AssumeYes {
		return GoInstallActionRun, nil
	}
	if opts.SelectGoInstallAction == nil {
		return GoInstallActionAbort, fmt.Errorf("%w: this Detent binary appears to be managed by go install. Rerun with --yes to run go install, or --from-release to switch to the release binary", ErrConfirmationRequired)
	}
	return opts.SelectGoInstallAction(status)
}

func (s *Service) runGoInstallUpdate(ctx context.Context, status Status, opts ApplyOptions) (Status, error) {
	target := goInstallTarget(status)
	status.Command = "go install " + target
	if err := s.cfg.CommandRunner(ctx, "go", []string{"install", target}, outputWriter(opts.Stdout), outputWriter(opts.Stderr)); err != nil {
		status.Action = ActionRefused
		status.Message = fmt.Sprintf("go install failed: %v", err)
		return status, err
	}

	versionOutput, err := s.cfg.BinaryVerifier(ctx, s.cfg.ExecutablePath)
	if err != nil {
		status.Action = ActionRefused
		status.Message = fmt.Sprintf("go install completed, but verifying Detent failed: %v", err)
		return status, err
	}
	installed := installedVersion(status, versionOutput)
	if err := verifyInstalledVersion(status, installed); err != nil {
		status.Action = ActionRefused
		status.Message = fmt.Sprintf("go install completed, but installed Detent version is not the planned update: %v", err)
		return status, err
	}

	status.Action = ActionUpdated
	status.Message = goInstallAppliedMessage(status, installed)
	return status, nil
}

func (s *Service) applyReleaseUpdate(ctx context.Context, status Status, release Release, opts ApplyOptions, releaseSwap bool, emitWarning bool) (Status, error) {
	if emitWarning {
		if err := writeReleaseSwapWarning(outputWriter(opts.Stderr), status); err != nil {
			status.Action = ActionRefused
			status.Message = err.Error()
			return status, err
		}
	}

	assets, err := selectReleaseAssets(release, s.cfg.GOOS, s.cfg.GOARCH, s.cfg.RequireChecksumSignature)
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
	if s.cfg.RequireChecksumSignature {
		checksumSignature, err := s.cfg.Client.Download(ctx, assets.ChecksumSignature.BrowserDownloadURL)
		if err != nil {
			status.Action = ActionRefused
			status.Message = err.Error()
			return status, err
		}
		if err := s.cfg.ChecksumSignatureVerifier(ctx, checksums, checksumSignature); err != nil {
			status.Action = ActionRefused
			status.Message = err.Error()
			return status, err
		}
	}
	archive, err := s.cfg.Client.Download(ctx, assets.Archive.BrowserDownloadURL)
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
	replacement := Replacement{
		Target:  s.cfg.ExecutablePath,
		Binary:  binary,
		Mode:    mode,
		GOOS:    s.cfg.GOOS,
		Context: ctx,
		Verify:  s.cfg.BinaryVerifier,
		Sign:    s.cfg.BinarySigner,
	}
	if releaseSwap {
		replacement.AfterReplace = func(_ context.Context, target string) error {
			return writeReleaseInstallLock(s.cfg.GOOS, DetectionOptions{
				HomeDir: s.cfg.HomeDir,
				Env:     s.cfg.Env,
			}, target)
		}
	}
	if err := ReplaceBinary(replacement); err != nil {
		status.Action = ActionRefused
		status.Message = err.Error()
		return status, err
	}

	status.Action = ActionUpdated
	status.Asset = assets.Archive.Name
	status.Message = updateAppliedMessage(status, s.cfg.GOOS, releaseSwap)
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
	return selectReleaseAssets(release, goos, goarch, true)
}

func selectReleaseAssets(release Release, goos string, goarch string, requireChecksumSignature bool) (ReleaseAssets, error) {
	archiveNames := archiveAssetNames(release.TagName, goos, goarch)
	checksumNames := checksumAssetNames(release.TagName)

	archive, ok := assetByName(release.Assets, archiveNames)
	if !ok {
		return ReleaseAssets{}, fmt.Errorf("release %s does not include an archive for %s/%s", release.TagName, goos, goarch)
	}
	checksum, checksumSignature, checksumFound, signatureFound := checksumAssetPair(release.Assets, checksumNames)
	if !checksumFound {
		return ReleaseAssets{}, fmt.Errorf("release %s does not include a checksum asset", release.TagName)
	}
	if requireChecksumSignature && !signatureFound {
		return ReleaseAssets{}, fmt.Errorf("release %s does not include a minisign signature for checksum asset %s", release.TagName, checksum.Name)
	}
	return ReleaseAssets{Archive: archive, Checksum: checksum, ChecksumSignature: checksumSignature}, nil
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

func VerifyChecksumSignature(ctx context.Context, checksums []byte, signature []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := VerifyMinisignSignature(defaultChecksumMinisignPublicKey, checksums, signature); err != nil {
		return fmt.Errorf("verify checksum signature: %w", err)
	}
	return nil
}

func checksumSignaturePublicKeyConfigured() bool {
	return strings.TrimSpace(defaultChecksumMinisignPublicKey) != ""
}

func VerifyMinisignSignature(publicKey string, message []byte, signature []byte) error {
	keyID, ed25519PublicKey, err := parseMinisignPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("parse minisign public key: %w", err)
	}
	signatureKeyID, messageSignature, trustedComment, globalSignature, err := parseMinisignSignature(signature)
	if err != nil {
		return fmt.Errorf("parse minisign signature: %w", err)
	}
	if !bytes.Equal(signatureKeyID, keyID) {
		return errors.New("minisign signature key id does not match pinned public key")
	}

	digest := blake2b.Sum512(message)
	if !ed25519.Verify(ed25519PublicKey, digest[:], messageSignature) {
		return errors.New("invalid minisign signature")
	}
	if !ed25519.Verify(ed25519PublicKey, minisignTrustedCommentMessage(messageSignature, trustedComment), globalSignature) {
		return errors.New("invalid minisign trusted comment signature")
	}
	return nil
}

func ExtractBinary(archive []byte, goos string) ([]byte, os.FileMode, error) {
	if goos == "windows" {
		return extractZipBinary(archive)
	}
	return extractTarGzipBinary(archive)
}

func ReplaceBinary(replacement Replacement) error {
	goos := firstNonEmpty(replacement.GOOS, runtime.GOOS)
	if goos == "windows" {
		return stageWindowsReplacement(replacement)
	}
	replacement.GOOS = goos
	return replaceBinaryNow(replacement)
}

func replaceBinaryNow(replacement Replacement) error {
	target := replacement.Target
	if strings.TrimSpace(target) == "" {
		return errors.New("target binary path is required")
	}
	mode := replacementMode(target, replacement.Mode)

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

	writeErr := writeBinaryTemp(temp, replacement.Binary, mode)
	if writeErr != nil {
		return writeErr
	}

	backupPath, err := backupBinary(target)
	if err != nil {
		return err
	}
	cleanupBackup := backupPath != ""
	defer func() {
		if cleanupBackup {
			removeFile(backupPath)
		}
	}()

	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("replace binary %s: %w", target, err)
	}
	cleanup = false
	if err := finalizeReplacement(replacement); err != nil {
		if rollbackErr := rollbackBinary(target, backupPath); rollbackErr != nil {
			return fmt.Errorf("%w; rollback failed: %w", err, rollbackErr)
		}
		cleanupBackup = false
		return err
	}
	return nil
}

func replacementMode(target string, fallback os.FileMode) os.FileMode {
	info, err := os.Stat(target)
	if err == nil {
		if mode := info.Mode().Perm(); mode != 0 {
			return mode
		}
	}
	if fallback != 0 {
		return fallback.Perm()
	}
	return 0o755
}

func backupBinary(target string) (string, error) {
	source, err := os.Open(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("open current binary for backup: %w", err)
	}
	defer source.Close()

	info, err := source.Stat()
	if err != nil {
		return "", fmt.Errorf("stat current binary for backup: %w", err)
	}
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	backup, err := os.CreateTemp(dir, "."+base+".rollback-*")
	if err != nil {
		return "", fmt.Errorf("create rollback binary: %w", err)
	}
	backupPath := backup.Name()
	if _, err := io.Copy(backup, source); err != nil {
		closeErr := backup.Close()
		removeFile(backupPath)
		if closeErr != nil {
			return "", fmt.Errorf("write rollback binary: %w; close rollback binary: %w", err, closeErr)
		}
		return "", fmt.Errorf("write rollback binary: %w", err)
	}
	if err := backup.Chmod(info.Mode().Perm()); err != nil {
		closeErr := backup.Close()
		removeFile(backupPath)
		if closeErr != nil {
			return "", fmt.Errorf("chmod rollback binary: %w; close rollback binary: %w", err, closeErr)
		}
		return "", fmt.Errorf("chmod rollback binary: %w", err)
	}
	if err := backup.Close(); err != nil {
		removeFile(backupPath)
		return "", fmt.Errorf("close rollback binary: %w", err)
	}
	return backupPath, nil
}

func finalizeReplacement(replacement Replacement) error {
	ctx := replacement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if replacement.GOOS == "darwin" {
		signed, err := hasValidCodeSignature(ctx, replacement.Target, replacement.CodeSignatureVerifier)
		if err != nil {
			return err
		}
		if !signed {
			signer := replacement.Sign
			if signer == nil {
				signer = signBinary
			}
			if err := signer(ctx, replacement.Target); err != nil {
				return err
			}
		}
	}
	if replacement.Verify != nil {
		if _, err := replacement.Verify(ctx, replacement.Target); err != nil {
			return err
		}
	}
	if replacement.AfterReplace != nil {
		if err := replacement.AfterReplace(ctx, replacement.Target); err != nil {
			return err
		}
	}
	return nil
}

func rollbackBinary(target string, backupPath string) error {
	if backupPath == "" {
		return nil
	}
	if err := os.Rename(backupPath, target); err != nil {
		return fmt.Errorf("restore previous binary %s: %w", target, err)
	}
	return nil
}

func stageWindowsReplacement(replacement Replacement) error {
	if strings.TrimSpace(replacement.Target) == "" {
		return errors.New("target binary path is required")
	}
	mode := replacement.Mode
	if mode == 0 {
		mode = 0o755
	}

	dir := filepath.Dir(replacement.Target)
	base := filepath.Base(replacement.Target)
	source, err := writeWindowsUpdateBinary(dir, base, replacement.Binary, mode)
	if err != nil {
		return err
	}
	cleanupSource := true
	defer func() {
		if cleanupSource {
			removeFile(source)
		}
	}()

	script, err := writeWindowsUpdateScript(dir, source, replacement.Target)
	if err != nil {
		return err
	}
	cleanupScript := true
	defer func() {
		if cleanupScript {
			removeFile(script)
		}
	}()

	starter := replacement.StartProcess
	if starter == nil {
		starter = startProcess
	}
	if err := starter("cmd.exe", []string{"/D", "/C", "start", "", "/B", "cmd.exe", "/D", "/C", script}); err != nil {
		return fmt.Errorf("start windows updater: %w", err)
	}
	if replacement.AfterReplace != nil {
		ctx := replacement.Context
		if ctx == nil {
			ctx = context.Background()
		}
		if err := replacement.AfterReplace(ctx, replacement.Target); err != nil {
			return err
		}
	}

	cleanupSource = false
	cleanupScript = false
	return nil
}

func writeWindowsUpdateBinary(dir string, base string, binary []byte, mode os.FileMode) (string, error) {
	temp, err := os.CreateTemp(dir, "."+base+".update-*")
	if err != nil {
		return "", fmt.Errorf("create update temp file: %w", err)
	}
	tempPath := temp.Name()
	if err := writeBinaryTemp(temp, binary, mode); err != nil {
		removeFile(tempPath)
		return "", err
	}
	return tempPath, nil
}

func writeWindowsUpdateScript(dir string, source string, target string) (string, error) {
	script, err := os.CreateTemp(dir, ".detent-update-*.cmd")
	if err != nil {
		return "", fmt.Errorf("create windows update script: %w", err)
	}
	scriptPath := script.Name()
	raw := windowsUpdateScript(source, target)
	if _, err := script.WriteString(raw); err != nil {
		closeErr := script.Close()
		removeFile(scriptPath)
		if closeErr != nil {
			return "", fmt.Errorf("write windows update script: %w; close update script: %w", err, closeErr)
		}
		return "", fmt.Errorf("write windows update script: %w", err)
	}
	if err := script.Chmod(0o700); err != nil {
		closeErr := script.Close()
		removeFile(scriptPath)
		if closeErr != nil {
			return "", fmt.Errorf("chmod windows update script: %w; close update script: %w", err, closeErr)
		}
		return "", fmt.Errorf("chmod windows update script: %w", err)
	}
	if err := script.Close(); err != nil {
		removeFile(scriptPath)
		return "", fmt.Errorf("close windows update script: %w", err)
	}
	return scriptPath, nil
}

func windowsUpdateScript(source string, target string) string {
	return fmt.Sprintf(`@echo off
setlocal DisableDelayedExpansion
set "source=%s"
set "target=%s"
set /a attempts=0
:retry
move /Y "%%source%%" "%%target%%" >nul 2>nul
if not exist "%%source%%" goto done
set /a attempts+=1
if %%attempts%% GEQ 60 exit /b 1
timeout /t 1 /nobreak >nul 2>nul
goto retry
:done
del "%%~f0" >nul 2>nul
exit /b 0
`, escapeBatchValue(source), escapeBatchValue(target))
}

func escapeBatchValue(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func startProcess(command string, args []string) error {
	return exec.Command(command, args...).Start()
}

func runCommand(ctx context.Context, command string, args []string, stdout io.Writer, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = outputWriter(stdout)
	cmd.Stderr = outputWriter(stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s: %w", strings.Join(append([]string{command}, args...), " "), err)
	}
	return nil
}

func verifyBinaryVersion(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("verify %s version: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func signBinary(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "codesign", "-s", "-", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func hasValidCodeSignature(ctx context.Context, path string, verifier CodeSignatureVerifier) (bool, error) {
	if verifier != nil {
		return verifier(ctx, path)
	}
	cmd := exec.CommandContext(ctx, "codesign", "--verify", "--strict", path)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	text := strings.TrimSpace(string(output))
	lower := strings.ToLower(text)
	if strings.Contains(lower, "code object is not signed") || strings.Contains(lower, "not signed at all") {
		return false, nil
	}
	return false, fmt.Errorf("verify codesign %s: %w: %s", path, err, text)
}

func outputWriter(writer io.Writer) io.Writer {
	if writer == nil {
		return io.Discard
	}
	return writer
}

func updateAppliedMessage(status Status, goos string, releaseSwap bool) string {
	if goos == "windows" {
		return fmt.Sprintf("Updated Detent from %s to %s. The replacement will finish after Detent exits.", status.CurrentVersion, status.LatestVersion)
	}
	if releaseSwap {
		return fmt.Sprintf(
			"Updated Detent from %s to %s from the release binary. This binary is now release-pinned instead of go-install-managed. %s",
			status.CurrentVersion,
			status.LatestVersion,
			restartNote(),
		)
	}
	return fmt.Sprintf("Updated Detent from %s to %s. %s", status.CurrentVersion, status.LatestVersion, restartNote())
}

func goInstallAppliedMessage(status Status, installed string) string {
	return fmt.Sprintf("Ran %s. Installed Detent version: %s. %s", status.Command, installed, restartNote())
}

func goInstallCommand(status Status) string {
	return "go install " + goInstallTarget(status)
}

func goInstallTarget(status Status) string {
	tag := strings.TrimSpace(status.LatestTag)
	if tag == "" {
		return moduleInstallTarget
	}
	version, err := parseVersion(tag)
	if err != nil || len(version.prerelease) == 0 {
		return moduleInstallTarget
	}
	return moduleInstallPackage + "@" + tag
}

func verifyInstalledVersion(status Status, installed string) error {
	expected := firstNonEmpty(status.LatestTag, status.LatestVersion)
	if expected == "" {
		return nil
	}
	cmp, err := CompareVersions(installed, expected)
	if err != nil {
		return fmt.Errorf("parse installed version %s: %w", installed, err)
	}
	if cmp != 0 {
		return fmt.Errorf("installed version %s does not match expected %s", installed, expected)
	}
	return nil
}

func installedVersion(status Status, versionOutput string) string {
	for _, line := range strings.Split(versionOutput, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "version") {
			if version := strings.TrimSpace(value); version != "" {
				return version
			}
		}
	}
	for _, line := range strings.Split(versionOutput, "\n") {
		if value := strings.TrimSpace(line); value != "" {
			return value
		}
	}
	return firstNonEmpty(status.LatestVersion, status.LatestTag, "unknown")
}

func restartNote() string {
	return "Restart Detent to use the new binary; any running orchestrator process keeps the old version until restarted."
}

func writeReleaseSwapWarning(out io.Writer, status Status) error {
	_, err := fmt.Fprintf(
		out,
		"WARNING: Switching to the release binary replaces %s and changes how Detent is managed. Future go install or go.mod upgrades will not track this binary; it will be pinned to GitHub release %s.\n",
		status.Binary,
		firstNonEmpty(status.LatestTag, status.LatestVersion),
	)
	return err
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

func parseMinisignPublicKey(raw string) ([]byte, ed25519.PublicKey, error) {
	encoded := firstMinisignPayloadLine(raw)
	if encoded == "" {
		return nil, nil, errors.New("public key is not configured")
	}
	packet, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, nil, err
	}
	if len(packet) != minisignPublicKeySize {
		return nil, nil, fmt.Errorf("public key packet has %d bytes, want %d", len(packet), minisignPublicKeySize)
	}
	if string(packet[:2]) != "Ed" {
		return nil, nil, fmt.Errorf("unsupported public key algorithm %q", packet[:2])
	}
	keyID := append([]byte(nil), packet[2:10]...)
	publicKey := append(ed25519.PublicKey(nil), packet[10:]...)
	return keyID, publicKey, nil
}

func parseMinisignSignature(raw []byte) ([]byte, []byte, string, []byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) < 4 {
		return nil, nil, "", nil, errors.New("signature file has fewer than 4 lines")
	}
	if !strings.HasPrefix(lines[0], "untrusted comment:") {
		return nil, nil, "", nil, errors.New("missing untrusted comment")
	}
	signaturePacket, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[1]))
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("decode signature packet: %w", err)
	}
	if len(signaturePacket) != minisignSignatureSize {
		return nil, nil, "", nil, fmt.Errorf("signature packet has %d bytes, want %d", len(signaturePacket), minisignSignatureSize)
	}
	if string(signaturePacket[:2]) != "ED" {
		return nil, nil, "", nil, fmt.Errorf("unsupported signature algorithm %q", signaturePacket[:2])
	}
	trustedComment, ok := strings.CutPrefix(lines[2], "trusted comment: ")
	if !ok {
		return nil, nil, "", nil, errors.New("missing trusted comment")
	}
	globalSignature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(lines[3]))
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("decode trusted comment signature: %w", err)
	}
	if len(globalSignature) != ed25519.SignatureSize {
		return nil, nil, "", nil, fmt.Errorf("trusted comment signature has %d bytes, want %d", len(globalSignature), ed25519.SignatureSize)
	}
	keyID := append([]byte(nil), signaturePacket[2:10]...)
	messageSignature := append([]byte(nil), signaturePacket[10:]...)
	return keyID, messageSignature, trustedComment, globalSignature, nil
}

func firstMinisignPayloadLine(raw string) string {
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "untrusted comment:") {
			continue
		}
		return line
	}
	return ""
}

func minisignTrustedCommentMessage(signature []byte, trustedComment string) []byte {
	message := make([]byte, 0, len(signature)+len(trustedComment))
	message = append(message, signature...)
	message = append(message, trustedComment...)
	return message
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

func checksumAssetPair(assets []Asset, checksumNames []string) (Asset, Asset, bool, bool) {
	var firstChecksum Asset
	for _, name := range checksumNames {
		checksum, ok := assetByName(assets, []string{name})
		if !ok {
			continue
		}
		if firstChecksum.Name == "" {
			firstChecksum = checksum
		}
		signature, ok := assetByName(assets, checksumSignatureAssetNames(checksum.Name))
		if ok {
			return checksum, signature, true, true
		}
	}
	return firstChecksum, Asset{}, firstChecksum.Name != "", false
}

func checksumSignatureAssetNames(checksumName string) []string {
	return []string{checksumName + ".minisig"}
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

func writeReleaseInstallLock(goos string, opts DetectionOptions, binary string) error {
	lockPath, ok := installLockPath(goos, opts)
	if !ok {
		return errors.New("resolve release install lock path: home directory is unavailable")
	}
	return writeInstallLock(lockPath, binary, time.Now().UTC())
}

func installLockPath(goos string, opts DetectionOptions) (string, bool) {
	if lockPath := envValue(opts.Env, "DETENT_INSTALL_LOCK"); lockPath != "" {
		return lockPath, true
	}
	if stateDir := envValue(opts.Env, "DETENT_STATE_DIR"); stateDir != "" {
		return filepath.Join(stateDir, "install.lock"), true
	}
	if goos == "windows" {
		if localAppData := envValue(opts.Env, "LOCALAPPDATA"); localAppData != "" {
			return filepath.Join(localAppData, "detent", "install.lock"), true
		}
	}
	if home := homeDir(opts.HomeDir, opts.Env); home != "" {
		return filepath.Join(home, ".detent", "install.lock"), true
	}
	return "", false
}

func writeInstallLock(path string, binary string, installedAt time.Time) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("install lock path is required")
	}
	if strings.TrimSpace(binary) == "" {
		return errors.New("install lock binary path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create install lock dir: %w", err)
	}
	base := filepath.Base(path)
	temp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("create install lock temp file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			removeFile(tempPath)
		}
	}()
	raw := fmt.Sprintf("binary=%s\ninstalled_at=%s\n", binary, installedAt.UTC().Format(time.RFC3339))
	if _, err := temp.WriteString(raw); err != nil {
		closeErr := temp.Close()
		if closeErr != nil {
			return fmt.Errorf("write install lock: %w; close install lock: %w", err, closeErr)
		}
		return fmt.Errorf("write install lock: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		closeErr := temp.Close()
		if closeErr != nil {
			return fmt.Errorf("chmod install lock: %w; close install lock: %w", err, closeErr)
		}
		return fmt.Errorf("chmod install lock: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close install lock: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace install lock: %w", err)
	}
	cleanup = false
	return nil
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
