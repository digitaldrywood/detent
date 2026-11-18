package hookproc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

type Lifetime struct {
	starts map[int]string
	reader *os.File
	writer *os.File
	dir    string
}

func (l *Lifetime) Attach(cmd *exec.Cmd) {
	cmd.ExtraFiles = append(cmd.ExtraFiles, nil, l.writer)
}

func (l *Lifetime) CloseWriter() error {
	if l.writer == nil {
		return nil
	}
	err := l.writer.Close()
	l.writer = nil
	return err
}

func (l *Lifetime) Close() error {
	if err := l.checkPath(); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		if err := os.Remove(l.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return errors.Join(l.CloseWriter(), l.reader.Close())
}

func (l *Lifetime) path() string { return filepath.Join(l.dir, "lifetime") }

func (l *Lifetime) checkPath() error {
	original, err := l.reader.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(l.path())
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) {
		return errors.New("hook lifetime descriptor path changed")
	}
	return nil
}

func (l *Lifetime) WaitClosed(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, procgroup.DefaultTerminationGrace)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		active, err := l.Active()
		if err != nil || !active {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.New("workspace hook descriptor still has an unidentified holder")
		case <-tick.C:
		}
	}
}
