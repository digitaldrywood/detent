package runnerauth

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
)

type File struct {
	Schema            int      `json:"schema"`
	HubURL            string   `json:"hub_url"`
	Identity          Identity `json:"identity"`
	Credential        string   `json:"credential"`
	PendingCredential string   `json:"pending_credential,omitempty"`
}

func ValidateHubURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return errors.New("runner Hub URL must be an absolute HTTPS URL without user information, query or fragment")
	}
	loopback := parsed.Hostname() == "localhost"
	if ip := net.ParseIP(parsed.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return errors.New("runner Hub URL requires HTTPS except on loopback")
	}
	return nil
}

func Initialize(path, hubURL string) (File, error) {
	return initializeBinding(path, hubURL, NewBinding())
}

func InitializeOnHost(path, hubURL, hostIdentityPath string) (File, error) {
	host, err := Load(hostIdentityPath)
	if err != nil {
		return File{}, err
	}
	if host.HubURL != strings.TrimRight(hubURL, "/") || host.Identity.OrganizationID == "" {
		return File{}, errors.New("shared host identity must already be enrolled with the same Hub")
	}
	binding := NewBinding()
	binding.MachineID = host.Identity.MachineID
	return initializeBinding(path, hubURL, binding)
}

func initializeBinding(path, hubURL string, binding Binding) (File, error) {
	if err := ValidateHubURL(hubURL); err != nil {
		return File{}, err
	}
	if !filepath.IsAbs(path) {
		return File{}, errors.New("runner identity file requires an absolute private path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return File{}, err
	}
	if err := validatePrivateDirectory(path); err != nil {
		return File{}, err
	}
	credential, err := apikey.GenerateToken()
	if err != nil {
		return File{}, err
	}
	value := File{Schema: 1, HubURL: strings.TrimRight(hubURL, "/"), Identity: Identity{Binding: binding}, Credential: credential}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return File{}, errors.New("runner identity could not be created; existing identities must not be overwritten")
	}
	err = json.NewEncoder(file).Encode(value)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return File{}, errors.New("runner identity could not be persisted")
	}
	if err := syncIdentityDirectory(path); err != nil {
		return File{}, err
	}
	return value, nil
}

func validatePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("runner identity requires an absolute path")
	}
	directory := filepath.Dir(path)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return errors.New("runner identity directory is unavailable")
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("runner identity directory must be private (0700)")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(directory))
	if err != nil || filepath.Clean(resolved) != filepath.Join(parent, filepath.Base(directory)) {
		return errors.New("runner identity directory must not be a symbolic link")
	}
	return nil
}

func Load(path string) (result File, resultErr error) {
	if err := validatePrivateDirectory(path); err != nil {
		return File{}, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 || info.Size() > 32<<10 {
		return File{}, errors.New("runner identity must be a private regular file (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return File{}, errors.New("runner identity is unavailable")
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var value File
	decoder := json.NewDecoder(io.LimitReader(file, (32<<10)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return File{}, errors.New("runner identity file is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return File{}, errors.New("runner identity file has extra content")
	}
	if value.Schema != 1 || !value.Identity.Valid() || !ValidCredential(value.Credential) || value.PendingCredential != "" && !ValidCredential(value.PendingCredential) {
		return File{}, errors.New("runner identity file is invalid")
	}
	if err := ValidateHubURL(value.HubURL); err != nil {
		return File{}, err
	}
	return value, nil
}

func Save(path string, value File) (resultErr error) {
	previous, err := Load(path)
	if err != nil {
		return err
	}
	if previous.Identity.Binding != value.Identity.Binding || previous.HubURL != value.HubURL || value.Schema != 1 || !ValidCredential(value.Credential) || value.PendingCredential != "" && !ValidCredential(value.PendingCredential) {
		return errors.New("runner identity binding cannot be replaced")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".runner-identity-*")
	if err != nil {
		return errors.New("runner identity update could not be created")
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, errors.New("runner identity temporary file cleanup failed"))
		}
	}()
	err = json.NewEncoder(file).Encode(value)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return errors.New("runner identity update could not be persisted")
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return errors.New("runner identity update could not be installed")
	}
	return syncIdentityDirectory(path)
}

func syncIdentityDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("runner identity directory could not be opened for synchronization")
	}
	if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
		return errors.New("runner identity directory could not be synchronized")
	}
	return nil
}
