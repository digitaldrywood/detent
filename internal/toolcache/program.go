package toolcache

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ProgramArgument = "__detent_go_cache"

type cacheRequest struct {
	ID       int64
	Command  string
	ActionID []byte
	OutputID []byte
	BodySize int64
}

type cacheResponse struct {
	ID            int64
	Err           string   `json:",omitempty"`
	KnownCommands []string `json:",omitempty"`
	Miss          bool     `json:",omitempty"`
	OutputID      []byte   `json:",omitempty"`
	Size          int64    `json:",omitempty"`
	DiskPath      string   `json:",omitempty"`
}

type cacheProgram struct {
	cache  *os.Root
	pins   *os.Root
	link   func(string, string) error
	rename func(string, string) error
}

func RunProgramFromEnvironment(input io.Reader, output io.Writer) error {
	return RunProgram(os.Getenv("GOCACHE"), os.Getenv("DETENT_WORKER_SCRATCH"), input, output)
}

func RunProgram(root string, scratch string, input io.Reader, output io.Writer) (err error) {
	program, err := newCacheProgram(root, scratch)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, program.close()) }()
	encoder := json.NewEncoder(output)
	if err := encoder.Encode(cacheResponse{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return fmt.Errorf("write cache capabilities: %w", err)
	}
	reader := bufio.NewReader(input)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read cache request: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var request cacheRequest
		if err := json.Unmarshal(line, &request); err != nil {
			return fmt.Errorf("decode cache request: %w", err)
		}
		response := cacheResponse{ID: request.ID}
		switch request.Command {
		case "get":
			response = program.get(request.ActionID)
		case "put":
			response, err = program.put(request, reader)
			if err != nil {
				return fmt.Errorf("store cache output: %w", err)
			}
		case "close":
		default:
			return errors.New("unsupported cache command")
		}
		response.ID = request.ID
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("write cache response: %w", err)
		}
		if request.Command == "close" {
			return nil
		}
	}
}

func newCacheProgram(root string, scratch string) (*cacheProgram, error) {
	if !filepath.IsAbs(root) || scratch != "" && !filepath.IsAbs(scratch) {
		return nil, errors.New("cache program requires absolute cache and scratch paths")
	}
	cache, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open managed cache: %w", err)
	}
	pins, err := openConsumerPins(scratch)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open consumer cache pins: %w", err), cache.Close())
	}
	return &cacheProgram{cache: cache, pins: pins, link: os.Link, rename: cache.Rename}, nil
}

func openConsumerPins(scratch string) (pins *os.Root, err error) {
	if scratch == "" {
		pins, err := os.MkdirTemp(os.TempDir(), "go-cache-pins-")
		if err != nil {
			return nil, err
		}
		return os.OpenRoot(pins)
	}
	dir, err := os.OpenRoot(scratch)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	const name = "go-cache-pins"
	if err := dir.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("consumer cache pins must be a directory")
	}
	return dir.OpenRoot(name)
}

func (p *cacheProgram) close() error {
	return errors.Join(p.pins.Close(), p.cache.Close())
}

func cacheName(id []byte, kind string) string {
	key := hex.EncodeToString(id)
	return key[:2] + "/" + key + "-" + kind
}

func (p *cacheProgram) get(action []byte) cacheResponse {
	miss := cacheResponse{Miss: true}
	if len(action) != sha256.Size {
		return miss
	}
	name := cacheName(action, "a")
	data, err := p.cache.ReadFile(name)
	if err != nil {
		return miss
	}
	var storedAction, output []byte
	var size, stamp int64
	if n, err := fmt.Sscanf(string(data), "v1 %x %x %d %d\n", &storedAction, &output, &size, &stamp); err != nil || n != 4 || !bytes.Equal(action, storedAction) || len(output) != sha256.Size || size < 0 {
		return miss
	}
	path, err := p.pin(cacheName(output, "d"), size, nil)
	if err != nil {
		return miss
	}
	if info, err := p.cache.Lstat(name); err == nil && time.Since(info.ModTime()) >= time.Hour {
		now := time.Now()
		if err := p.cache.Chtimes(name, now, now); err != nil && !os.IsNotExist(err) {
			return miss
		}
	}
	return cacheResponse{OutputID: output, Size: size, DiskPath: path}
}

func (p *cacheProgram) pin(name string, size int64, file *os.File) (path string, err error) {
	base, _, _ := strings.Cut(filepath.Base(name), ".")
	path = filepath.Join(p.pins.Name(), base)
	if info := p.pinnedOutput(base, size); info != nil {
		return path, p.touchCachePin(base, info)
	}
	staged := base + "." + rand.Text()
	defer func() {
		if removeErr := p.pins.Remove(staged); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := p.stagePin(name, staged, size, file); err != nil {
		return "", err
	}
	linkErr := p.pins.Link(staged, base)
	if linkErr != nil {
		if info := p.pinnedOutput(base, size); info != nil {
			return path, p.touchCachePin(base, info)
		}
		if err := p.pins.Rename(staged, base); err != nil {
			if info := p.pinnedOutput(base, size); info != nil {
				return path, p.touchCachePin(base, info)
			}
			return "", errors.Join(linkErr, err)
		}
	}
	return path, nil
}

func (p *cacheProgram) pinnedOutput(name string, size int64) os.FileInfo {
	info, err := p.pins.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil
	}
	return info
}

func (p *cacheProgram) stagePin(name, staged string, size int64, file *os.File) (err error) {
	if file == nil {
		file, err = p.cache.Open(name)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, file.Close()) }()
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.Join(errors.New("cache output incomplete"), err)
	}
	if err := p.link(filepath.Join(p.cache.Name(), name), filepath.Join(p.pins.Name(), staged)); err == nil {
		linked, err := p.pins.Lstat(staged)
		if err == nil && linked.Mode().IsRegular() && os.SameFile(info, linked) {
			return p.touchCachePin(staged, linked)
		}
		if err := p.pins.Remove(staged); err != nil {
			return err
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	copy, err := p.pins.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := io.Copy(copy, file)
	if err := errors.Join(writeErr, copy.Close()); err != nil {
		return err
	}
	if written != size {
		return errors.New("cache output copy incomplete")
	}
	return nil
}

func (p *cacheProgram) touchCachePin(name string, info os.FileInfo) error {
	now := time.Now()
	if now.Sub(info.ModTime()) < time.Hour {
		return nil
	}
	return p.pins.Chtimes(name, now, now)
}

func (p *cacheProgram) put(request cacheRequest, reader *bufio.Reader) (response cacheResponse, err error) {
	if len(request.ActionID) != sha256.Size || len(request.OutputID) != sha256.Size || request.BodySize < 0 || request.BodySize > (1<<63-1)/4 {
		return response, errors.New("invalid cache put request")
	}
	name := cacheName(request.OutputID, "d")
	if err := p.cache.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return response, err
	}
	staged := name + "." + rand.Text()
	file, err := p.cache.OpenFile(staged, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return response, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	defer func() {
		if removeErr := p.cache.Remove(staged); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}()
	hash := sha256.New()
	var writeErr error
	var written int64
	if request.BodySize > 0 {
		var opening byte
		for {
			opening, writeErr = reader.ReadByte()
			if writeErr != nil || !strings.ContainsRune(" \r\n\t", rune(opening)) {
				break
			}
		}
		if writeErr == nil && opening != '"' {
			writeErr = errors.New("cache body is not a JSON string")
		}
		if writeErr == nil {
			body := base64.NewDecoder(base64.StdEncoding, io.LimitReader(reader, (request.BodySize+2)/3*4))
			written, writeErr = io.Copy(io.MultiWriter(file, hash), body)
		}
		if writeErr == nil {
			closing, readErr := reader.ReadByte()
			writeErr = readErr
			if writeErr == nil && closing != '"' {
				writeErr = errors.New("cache body is not terminated")
			}
		}
	}
	if writeErr != nil {
		return response, writeErr
	}
	if written != request.BodySize || !bytes.Equal(hash.Sum(nil), request.OutputID) {
		return response, errors.New("cache body does not match output ID and size")
	}
	path, err := p.pin(staged, request.BodySize, file)
	if err != nil {
		return response, err
	}
	response = cacheResponse{DiskPath: path}
	if err := p.rename(staged, name); err != nil {
		if os.IsNotExist(err) {
			return response, nil
		}
		return response, err
	}
	index := cacheName(request.ActionID, "a")
	if err := p.cache.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		return response, err
	}
	stagedIndex := index + "." + rand.Text()
	defer func() {
		if removeErr := p.cache.Remove(stagedIndex); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
	}()
	data := fmt.Sprintf("v1 %x %x %20d %20d\n", request.ActionID, request.OutputID, request.BodySize, time.Now().UnixNano())
	if err := p.cache.WriteFile(stagedIndex, []byte(data), 0o600); err != nil {
		return response, err
	}
	if err := p.rename(stagedIndex, index); err != nil && !os.IsNotExist(err) {
		return response, err
	}
	return response, nil
}
