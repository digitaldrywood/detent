package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/digitaldrywood/detent/internal/service"
)

const serviceLogRetentionInterval = time.Second

type serviceLog struct {
	path   string
	stream *os.File
}

func startServiceLogRetention(ctx context.Context, manager, configPath string, rotation logRotation, stdout, stderr *os.File) func() {
	if manager != string(service.ManagerLaunchd) && manager != string(service.ManagerSystemd) && manager != string(service.ManagerScheduledTask) {
		return func() {}
	}
	if rotation.MaxSizeBytes <= 0 {
		rotation.MaxSizeBytes = int64(defaultRuntimeLogMaxSizeBytes)
	}
	if rotation.MaxBackups <= 0 {
		rotation.MaxBackups = defaultRuntimeLogMaxBackups
	}
	outPath, errPath := service.LogPaths(service.Config{ConfigPath: configPath})
	logs := []serviceLog{{path: outPath, stream: stdout}, {path: errPath, stream: stderr}}
	retain := func() {
		for _, log := range logs {
			if err := log.retain(rotation); err != nil {
				slog.WarnContext(ctx, "retain service log failed", "path", log.path, "error", err)
			}
		}
	}
	retain()
	retentionCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(serviceLogRetentionInterval)
		defer ticker.Stop()
		for {
			select {
			case <-retentionCtx.Done():
				return
			case <-ticker.C:
				retain()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (log serviceLog) retain(rotation logRotation) error {
	return log.retainWithCopy(rotation, io.Copy)
}

func (log serviceLog) retainWithCopy(rotation logRotation, copySnapshot func(io.Writer, io.Reader) (int64, error)) (retErr error) {
	if log.stream == nil {
		return nil
	}
	streamInfo, err := log.stream.Stat()
	if err != nil {
		return err
	}
	if !streamInfo.Mode().IsRegular() {
		return nil
	}
	info, err := os.Lstat(log.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, streamInfo) || info.Size() <= rotation.MaxSizeBytes {
		return nil
	}
	source, err := os.Open(log.path)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, source.Close())
	}()
	sourceInfo, err := source.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(sourceInfo, streamInfo) {
		return nil
	}
	backup, err := os.CreateTemp(filepath.Dir(log.path), filepath.Base(log.path)+".snapshot-*")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(backup.Name()); !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, err)
		}
	}()
	tail := io.NewSectionReader(source, max(0, sourceInfo.Size()-rotation.MaxSizeBytes), rotation.MaxSizeBytes)
	_, copyErr := copySnapshot(backup, tail)
	if err := errors.Join(copyErr, backup.Close()); err != nil {
		return fmt.Errorf("snapshot service log: %w", err)
	}
	if err := rotateLogBackups(log.path, rotation.MaxBackups); err != nil {
		return err
	}
	if err := os.Rename(backup.Name(), rotatedLogPath(log.path, 1)); err != nil {
		return err
	}
	return log.stream.Truncate(0)
}

func openScheduledTaskLogs(configPath string) (*os.File, *os.File, error) {
	outPath, errPath := service.LogPaths(service.Config{ConfigPath: configPath})
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create service log directory: %w", err)
	}
	stdout, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open service stdout: %w", err)
	}
	stderr, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("open service stderr: %w", err), stdout.Close())
	}
	return stdout, stderr, nil
}
