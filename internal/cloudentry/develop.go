package cloudentry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

func prepareDevelopState(ctx context.Context, cfg Config) (Config, func() error, error) {
	if !strings.HasPrefix(cfg.Build.Version, "develop-") || cfg.PublicURL != "https://staging.cloud.detent.build" {
		return cfg, nil, nil
	}
	if cfg.Allocation == nil {
		return cfg, nil, errors.New("develop staging requires managed tenant allocation for disposable state")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return cfg, nil, err
	}
	absolute, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return cfg, nil, err
	}
	source, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return cfg, nil, err
	}
	tenantRoot := cfg.Allocation.TenantRoot
	if resolved, err := filepath.EvalSymlinks(tenantRoot); err == nil {
		tenantRoot = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, nil, err
	}
	lock, err := instancelock.Acquire(filepath.Join(source, "registry.db.lock"))
	if err != nil {
		return cfg, nil, fmt.Errorf("acquire release state ownership for develop preview: %w", err)
	}
	root := filepath.Join(source, ".develop")
	if relative, err := filepath.Rel(root, tenantRoot); err != nil || relative == "." || filepath.IsLocal(relative) {
		return cfg, nil, errors.Join(errors.New("release tenant root must be outside disposable develop state"), lock.Close())
	}
	if relative, err := filepath.Rel(tenantRoot, root); err != nil || relative == "." || filepath.IsLocal(relative) {
		return cfg, nil, errors.Join(errors.New("disposable develop state must be outside the release tenant root"), lock.Close())
	}
	closeState := sync.OnceValue(func() error { return errors.Join(os.RemoveAll(root), lock.Close()) })
	failed := func(err error) (Config, func() error, error) {
		return cfg, nil, errors.Join(err, closeState())
	}
	if err := os.RemoveAll(root); err != nil {
		return failed(err)
	}
	entry := filepath.Join(root, "entry")
	if err := os.MkdirAll(entry, 0o700); err != nil {
		return failed(err)
	}
	for _, name := range []string{"registry.db", "auth.db"} {
		path := filepath.Join(source, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return failed(err)
		}
		if err := snapshotDevelopDatabase(ctx, path, filepath.Join(entry, name), name == "registry.db"); err != nil {
			return failed(fmt.Errorf("snapshot release %s: %w", name, err))
		}
	}
	allocation := *cfg.Allocation
	allocation.TenantRoot = filepath.Join(root, "tenants")
	allocation.SocketRoot = filepath.Join(cfg.Allocation.SocketRoot, "develop")
	if err := copyDevelopTenants(ctx, tenantRoot, allocation.TenantRoot); err != nil {
		return failed(err)
	}
	if err := os.MkdirAll(allocation.SocketRoot, 0o700); err != nil {
		return failed(err)
	}
	cfg.StateDir, cfg.Allocation = entry, &allocation
	cfg.Logger.InfoContext(ctx, "develop staging uses disposable release state", "source", source, "preview", root)
	return cfg, closeState, nil
}

func copyDevelopTenants(ctx context.Context, source, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if entry.Type()&os.ModeSocket != 0 || strings.HasSuffix(path, ".lock") || strings.HasSuffix(path, ".db-wal") || strings.HasSuffix(path, ".db-shm") {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("develop state must contain regular files: %s", path)
		}
		if strings.HasSuffix(path, ".db") {
			return snapshotDevelopDatabase(ctx, path, target, false)
		}
		return copyDevelopFile(path, target)
	})
}

func copyDevelopFile(source, destination string) (resultErr error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, output.Close()) }()
	_, err = io.Copy(output, input)
	return err
}

func snapshotDevelopDatabase(ctx context.Context, source, destination string, owned bool) (resultErr error) {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(fmt.Errorf("develop snapshot requires a regular database: %s", source), err)
	}
	if !owned {
		lock, err := instancelock.Acquire(source + ".lock")
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	}
	dsn, err := url.Parse(storeDSN(source))
	if err != nil {
		return err
	}
	query := dsn.Query()
	query.Del("_pragma")
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	if err := os.WriteFile(destination, nil, 0o600); err != nil {
		return err
	}
	connection, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	return connection.Raw(func(driver any) error {
		backuper, ok := driver.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite driver does not support develop snapshots")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		for more := true; more; {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, backup.Finish())
			}
			more, err = backup.Step(256)
			if err != nil {
				return errors.Join(err, backup.Finish())
			}
		}
		return backup.Finish()
	})
}

func (s *Service) routeDevelopTenants(ctx context.Context) error {
	if s.closeDevelopState == nil {
		return nil
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		return err
	}
	for _, organization := range organizations {
		if organization.State == "deleted" {
			continue
		}
		if !organization.Managed {
			return errors.New("develop staging preview cannot route an unmanaged release tenant")
		}
		endpoint := "unix:" + filepath.Join(s.config.Allocation.SocketRoot, organization.ID+".sock")
		if _, err := s.registry.store.db.ExecContext(ctx, "UPDATE organizations SET endpoint = ? WHERE id = ?", endpoint, organization.ID); err != nil {
			return err
		}
	}
	return nil
}
