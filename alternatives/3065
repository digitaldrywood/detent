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
	cache *os.Root
	pins  string
	link  func(string, string) error
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
	if !filepath.IsAbs(root) || !filepath.IsAbs(scratch) {
		return nil, errors.New("cache program requires absolute cache and scratch paths")
	}
	cache, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open managed cache: %w", err)
	}
	pins, err := os.MkdirTemp(scratch, "go-cache-pins-")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create cache pins: %w", err), cache.Close())
	}
	return &cacheProgram{cache: cache, pins: pins, link: os.Link}, nil
}

func (p *cacheProgram) close() error {
	return errors.Join(os.RemoveAll(p.pins), p.cache.Close())
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
	path, err := p.pin(cacheName(output, "d"), size)
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

func (p *cacheProgram) pin(name string, size int64) (path string, err error) {
	base, _, _ := strings.Cut(filepath.Base(name), ".")
	path = filepath.Join(p.pins, base)
	defer func() {
		if err != nil {
			if removeErr := os.Remove(filepath.Join(p.pins, base)); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() == size {
		return path, touchCachePin(path, info)
	}
	file, err := p.cache.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return "", errors.Join(errors.New("cache output incomplete"), err)
	}
	if err := p.link(filepath.Join(p.cache.Name(), name), path); err == nil {
		linked, err := os.Lstat(path)
		if err == nil && linked.Mode().IsRegular() && os.SameFile(info, linked) {
			return path, touchCachePin(path, linked)
		}
		if err := os.Remove(path); err != nil {
			return "", err
		}
	}
	copy, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	written, writeErr := io.Copy(copy, file)
	if err := errors.Join(writeErr, copy.Close()); err != nil {
		return "", err
	}
	if written != size {
		return "", errors.New("cache output copy incomplete")
	}
	return path, nil
}

func touchCachePin(path string, info os.FileInfo) error {
	now := time.Now()
	if now.Sub(info.ModTime()) < time.Hour {
		return nil
	}
	return os.Chtimes(path, now, now)
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
	file, err := p.cache.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return response, err
	}
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
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return response, err
	}
	if written != request.BodySize || !bytes.Equal(hash.Sum(nil), request.OutputID) {
		return response, errors.New("cache body does not match output ID and size")
	}
	path, err := p.pin(staged, request.BodySize)
	if err != nil {
		return response, err
	}
	if err := p.cache.Rename(staged, name); err != nil {
		return response, err
	}
	index := cacheName(request.ActionID, "a")
	if err := p.cache.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		return response, err
	}
	stagedIndex := index + "." + rand.Text()
	data := fmt.Sprintf("v1 %x %x %20d %20d\n", request.ActionID, request.OutputID, request.BodySize, time.Now().UnixNano())
	if err := p.cache.WriteFile(stagedIndex, []byte(data), 0o600); err != nil {
		return response, err
	}
	if err := p.cache.Rename(stagedIndex, index); err != nil {
		return response, errors.Join(err, p.cache.Remove(stagedIndex))
	}
	return cacheResponse{DiskPath: path}, nil
}
