package runnerauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type routingCache struct {
	Schema    int             `json:"schema"`
	HubURL    string          `json:"hub_url"`
	MachineID string          `json:"machine_id"`
	Snapshot  RoutingSnapshot `json:"snapshot"`
}

func RoutingCachePath(identityPath string) string {
	return identityPath + ".routing.json"
}

func validateLocalHostServices(services []string) error {
	for _, service := range services {
		path, ok := strings.CutPrefix(service, "unix:")
		if !ok {
			continue
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("host service %q cannot be inspected: %w", service, err)
		}
		if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("host service %q is a directory or symbolic link", service)
		}
	}
	return nil
}

func SaveRoutingCache(identityPath string, snapshot RoutingSnapshot) (resultErr error) {
	identity, err := Load(identityPath)
	if err != nil {
		return err
	}
	snapshot.Routing = snapshot.Routing.Normalized()
	if snapshot.RunnerID != identity.Identity.RunnerID || snapshot.Revision < 1 {
		return errors.New("runner routing cache belongs to another runner or has no revision")
	}
	if err := snapshot.Routing.Validate(); err != nil {
		return err
	}
	if err := validateLocalHostServices(snapshot.Routing.HostServices); err != nil {
		return err
	}
	path := RoutingCachePath(identityPath)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return errors.New("runner routing cache must be a private regular file (0600)")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	value := routingCache{Schema: 1, HubURL: identity.HubURL, MachineID: string(identity.Identity.MachineID), Snapshot: snapshot}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 64<<10 {
		return errors.New("runner routing cache exceeds its size limit")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".runner-routing-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	_, err = file.Write(encoded)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncIdentityDirectory(identityPath)
}

func LoadRoutingCache(identityPath string) (snapshot RoutingSnapshot, resultErr error) {
	identity, err := Load(identityPath)
	if err != nil {
		return RoutingSnapshot{}, err
	}
	path := RoutingCachePath(identityPath)
	info, err := os.Lstat(path)
	if err != nil {
		return RoutingSnapshot{}, err
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 || info.Size() > 64<<10 {
		return RoutingSnapshot{}, errors.New("runner routing cache must be a private regular file (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return RoutingSnapshot{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var value routingCache
	decoder := json.NewDecoder(io.LimitReader(file, (64<<10)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return RoutingSnapshot{}, errors.New("runner routing cache is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RoutingSnapshot{}, errors.New("runner routing cache has extra content")
	}
	if value.Schema != 1 || value.HubURL != identity.HubURL || value.MachineID != string(identity.Identity.MachineID) || value.Snapshot.RunnerID != identity.Identity.RunnerID || value.Snapshot.Revision < 1 {
		return RoutingSnapshot{}, errors.New("runner routing cache does not match the runner identity")
	}
	value.Snapshot.Routing = value.Snapshot.Routing.Normalized()
	if err := value.Snapshot.Routing.Validate(); err != nil {
		return RoutingSnapshot{}, err
	}
	if err := validateLocalHostServices(value.Snapshot.Routing.HostServices); err != nil {
		return RoutingSnapshot{}, err
	}
	return value.Snapshot, nil
}
