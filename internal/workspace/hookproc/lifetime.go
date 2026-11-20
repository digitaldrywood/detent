package hookproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/digitaldrywood/detent/internal/procstart"
)

const defaultProcessTerminationGrace = 250 * time.Millisecond

type Lifetime struct {
	instances          hookProcessInstances
	starts             map[int]string
	unidentifiedWriter bool
	witnesses          map[int]string
	reader             *os.File
	dir                string
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
	return l.reader.Close()
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

func (l *Lifetime) recordWitness(pid int, start string) {
	if l.witnesses == nil {
		l.witnesses = make(map[int]string)
	}
	l.witnesses[pid] = start
}

func (l *Lifetime) verifyWriterExits() error {
	if l.unidentifiedWriter {
		return errors.New("observed hook descriptor writer lacks a verified process identity")
	}
	for pid, start := range l.witnesses {
		alive, err := procstart.Matches(pid, start)
		if err != nil {
			return err
		}
		if alive {
			return errors.New("observed hook process is still running")
		}
	}
	return nil
}

func (l *Lifetime) WaitClosed(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, defaultProcessTerminationGrace)
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
