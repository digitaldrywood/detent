//go:build unix

package hookproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func (l *Lifetime) darwinWriters(ctx context.Context, target int) ([]int, error) {
	if err := l.checkPath(); err != nil {
		return nil, err
	}
	self := os.Getpid()
	readerFD := strconv.FormatUint(uint64(l.reader.Fd()), 10)
	selector, value := "-u", strconv.Itoa(os.Geteuid())
	if target > 0 {
		selector, value = "-p", strconv.Itoa(self)+","+strconv.Itoa(target)
	}
	data, err := lsofOutput(ctx, "-O", "-nP", "-w", "-a", selector, value, "-d", "4,"+readerFD, "-F0pfaitn")
	if err != nil && (!lsofNoMatches(err) || ctx.Err() != nil) {
		return nil, fmt.Errorf("list hook descriptors: %w", err)
	}

	return parseLsofFIFOWriterIDs(string(data), self, readerFD)
}

func parseLsofFIFOWriterIDs(output string, self int, readerFD string) ([]int, error) {
	type fifo struct{ inode, name string }
	var marker fifo
	writers := make(map[int]fifo)
	var pid int
	for record := range strings.SplitSeq(output, "\x00\n") {
		first, rest, _ := strings.Cut(record, "\x00")
		if value, ok := strings.CutPrefix(first, "p"); ok {
			pid = 0
			if parsed, err := strconv.Atoi(value); err == nil {
				pid = parsed
			}
			continue
		}
		var file fifo
		var access, kind string
		for field := range strings.SplitSeq(rest, "\x00") {
			if field == "" {
				continue
			}
			switch field[0] {
			case 'a':
				access = field[1:]
			case 'i':
				file.inode = field[1:]
			case 'n':
				file.name = field[1:]
			case 't':
				kind = field[1:]
			}
		}
		if kind != "FIFO" || file.inode == "" || file.name == "" {
			continue
		}
		switch {
		case pid == self && first == "f"+readerFD && access == "r":
			marker = file
		case pid > 0 && pid != self && first == "f4" && access == "w":
			writers[pid] = file
		}
	}
	if marker == (fifo{}) {
		return nil, errors.New("hook lifetime reader missing from lsof output")
	}
	var pids []int
	for pid, file := range writers {
		if file == marker {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
