// Package workspacefiles is the runner's read-only file service for a
// workspace session's files channel (decisions section 18.4).
//
// Everything here exists to make one promise true: what a reader sees through
// this surface is bounded by what they could already read, and never widens to
// what the runner account can read. Containment is therefore enforced at open
// time on the canonical path of what was actually opened, not on the request
// string -- a check on the string alone loses to a symlink swapped in between
// the check and the open, and to a link that points at an in-tree secret.
//
// Writes are out of scope for this slice. Editing happens through the model or
// the reader's own checkout.
package workspacefiles

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Error is a refusal in the files channel's own vocabulary. The code is what
// reaches the person; the wrapped cause is for the runner's log, because the
// reason a path was refused can itself disclose what is there.
type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string {
	if e.cause == nil {
		return e.Code
	}
	return e.Code + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error { return e.cause }

func refuse(code string, cause error) error { return &Error{Code: code, cause: cause} }

// ErrorCode reports the channel code for an error, or the empty string when it
// is not one of ours.
func ErrorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// Service serves one worktree.
type Service struct {
	// root confines every open to the worktree. It is a directory handle, so
	// a worktree moved or replaced under the service keeps referring to what
	// was opened rather than following the new path.
	root *os.Root
	// path is the worktree's canonical path, used for the post-open
	// containment proof and for the git calls that decide what is ignored.
	path string
	// device is the worktree root's filesystem. Anything on a different one
	// is a mount point under the worktree and is refused: os.Root does not
	// stop traversal of filesystem boundaries, and a bind mount is exactly
	// how a worktree gets a window onto something else.
	device uint64
	// hasDevice reports whether the platform could answer that question. A
	// platform that cannot is not a reason to refuse everything, but it is a
	// reason to say so here rather than silently skipping the check.
	hasDevice bool
	deny      workspacesession.Denylist
	// hasGit records whether a git binary was found on PATH. Without one
	// nothing is reported as ignored: showing a file that should have been
	// hidden is a cosmetic failure, and hiding one that should be visible is
	// not.
	hasGit bool
}

// Open prepares a service for a worktree.
func Open(worktree string, deny workspacesession.Denylist) (*Service, error) {
	canonical, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree %q: %w", worktree, err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, fmt.Errorf("open worktree %q: %w", canonical, err)
	}
	info, err := root.Stat(".")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stat worktree %q: %w", canonical, err), root.Close())
	}
	device, hasDevice := fileDevice(info)
	service := &Service{root: root, path: canonical, device: device, hasDevice: hasDevice, deny: deny}
	if _, err := exec.LookPath("git"); err == nil {
		service.hasGit = true
	}
	return service, nil
}

// Close releases the worktree handle.
func (s *Service) Close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

// Path reports the worktree's canonical path.
func (s *Service) Path() string { return s.path }

// List answers a list frame with one page of a directory.
//
// The cursor is the name the next page starts after rather than an offset: a
// directory that changes between pages would otherwise skip or repeat entries,
// and a reader watching a build would see exactly that.
//
// Paging happens before the .gitignore filter, so a page may come back with
// fewer than DirectoryPage rows while still reporting a next cursor. That is
// the honest shape: the alternative is reading the whole directory and
// classifying all of it before answering, which is unbounded work for a
// node_modules-sized tree.
func (s *Service) List(ctx context.Context, request workspacesession.FilesRequest) (listed workspacesession.FilesListed, resultErr error) {
	relative, err := s.resolve(request.Path)
	if err != nil {
		return workspacesession.FilesListed{}, err
	}
	handle, info, err := s.openDirectory(relative)
	if err != nil {
		return workspacesession.FilesListed{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	if err := s.requireSameDevice(info); err != nil {
		return workspacesession.FilesListed{}, err
	}
	entries, err := nextPage(handle, request.Cursor)
	if err != nil {
		return workspacesession.FilesListed{}, refuse(workspacesession.CodeForbidden, err)
	}
	listed = workspacesession.FilesListed{Path: relative, Entries: []workspacesession.FilesEntry{}}
	page := make([]workspacesession.FilesEntry, 0, workspacesession.DirectoryPage)
	paths := make([]string, 0, workspacesession.DirectoryPage)
	for _, entry := range entries {
		if len(page) == workspacesession.DirectoryPage {
			listed.NextCursor = page[len(page)-1].Name
			break
		}
		row, childPath, ok := s.describe(relative, entry)
		if !ok {
			continue
		}
		page = append(page, row)
		paths = append(paths, childPath)
	}
	ignored := s.ignoredPaths(ctx, paths)
	for index, row := range page {
		row.Ignored = ignored[paths[index]]
		if row.Ignored && !request.ShowIgnored {
			continue
		}
		listed.Entries = append(listed.Entries, row)
	}
	return listed, nil
}

// readDirBatch is how many entries one ReadDir call returns while a page is
// assembled.
const readDirBatch = 256

// nextPage reads a directory in bounded batches and keeps only the
// DirectoryPage+1 smallest listable names after cursor, sorted. Memory stays
// proportional to one page however large the directory is; the extra entry is
// what tells the caller another page exists.
func nextPage(handle *os.File, cursor string) ([]fs.DirEntry, error) {
	keep := workspacesession.DirectoryPage + 1
	kept := make([]fs.DirEntry, 0, 2*keep)
	byName := func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) }
	for {
		batch, err := handle.ReadDir(readDirBatch)
		for _, entry := range batch {
			if cursor != "" && entry.Name() <= cursor {
				continue
			}
			if !listable(entry.Type()) {
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) >= 2*keep {
			slices.SortFunc(kept, byName)
			kept = kept[:keep]
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(kept, byName)
	if len(kept) > keep {
		kept = kept[:keep]
	}
	return kept, nil
}

// listable reports whether an entry type is one a listing shows: symlinks,
// directories and regular files.
func listable(mode fs.FileMode) bool {
	return mode&fs.ModeSymlink != 0 || mode.IsDir() || mode.IsRegular()
}

// describe builds one listing row. A symlink is listed as a symlink with no
// target: naming what it points at would leak a path outside the worktree to a
// reader who may not see it, and reading it answers forbidden anyway.
func (s *Service) describe(parent string, entry fs.DirEntry) (workspacesession.FilesEntry, string, bool) {
	child := entry.Name()
	if parent != "" {
		child = parent + "/" + entry.Name()
	}
	row := workspacesession.FilesEntry{Name: entry.Name(), Denied: s.deny.Denied(child)}
	switch {
	case entry.Type()&fs.ModeSymlink != 0:
		row.Kind = workspacesession.KindSymlink
	case entry.IsDir():
		row.Kind = workspacesession.KindDir
	case entry.Type().IsRegular():
		row.Kind = workspacesession.KindFile
	default:
		// Devices, sockets and FIFOs are not files a reader may open, and
		// listing them invites a read that would block on one.
		return workspacesession.FilesEntry{}, "", false
	}
	if info, err := entry.Info(); err == nil {
		row.ModifiedAt = info.ModTime().UTC()
		if row.Kind == workspacesession.KindFile {
			row.Size = info.Size()
		}
	}
	return row, child, true
}

// Stat answers a stat frame.
func (s *Service) Stat(_ context.Context, request workspacesession.FilesRequest) (workspacesession.FilesStat, error) {
	relative, err := s.resolve(request.Path)
	if err != nil {
		return workspacesession.FilesStat{}, err
	}
	info, err := s.lstat(relative)
	if err != nil {
		return workspacesession.FilesStat{}, err
	}
	result := workspacesession.FilesStat{
		Path: relative, ModifiedAt: info.ModTime().UTC(), Denied: s.deny.Denied(relative),
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		result.Kind = workspacesession.KindSymlink
	case info.IsDir():
		result.Kind = workspacesession.KindDir
	case info.Mode().IsRegular():
		result.Kind = workspacesession.KindFile
		result.Size = info.Size()
		result.MIME = mimeForPath(relative)
	default:
		return workspacesession.FilesStat{}, refuse(workspacesession.CodeForbidden, errors.New("not a regular file or directory"))
	}
	return result, nil
}

// Read answers a read frame. It is where the whole containment argument has to
// hold, so the order matters: the denylist first, then an lstat that refuses a
// symlink before anything is opened, then the open itself with O_NOFOLLOW on
// the final component, then a proof that what was opened is a regular file on
// the worktree's own filesystem.
func (s *Service) Read(_ context.Context, request workspacesession.FilesRequest) (content workspacesession.FilesContent, resultErr error) {
	relative, err := s.resolve(request.Path)
	if err != nil {
		return workspacesession.FilesContent{}, err
	}
	if relative == "" {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, errors.New("the worktree root is not a file"))
	}
	if s.deny.Denied(relative) {
		// A denied entry is listed and never read. Answering "denied" rather
		// than "not found" is deliberate: the reader may see that the file
		// exists, which they could infer from the listing anyway, and may not
		// see what is in it.
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeDenied, errors.New(relative))
	}
	info, err := s.lstat(relative)
	if err != nil {
		return workspacesession.FilesContent{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, errors.New("symbolic links are not read"))
	}
	if !info.Mode().IsRegular() {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, errors.New("not a regular file"))
	}
	handle, err := s.root.OpenFile(relative, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return workspacesession.FilesContent{}, translateOpenError(err)
	}
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	opened, err := handle.Stat()
	if err != nil {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, err)
	}
	// The canonical path of what was actually opened is the containment
	// proof. os.Root already refuses a name that leaves the root; this
	// catches the case it explicitly does not cover, a mount point under the
	// worktree, and re-proves the type after the open rather than before.
	if !opened.Mode().IsRegular() {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, errors.New("not a regular file"))
	}
	if err := s.requireSameDevice(opened); err != nil {
		return workspacesession.FilesContent{}, err
	}
	offset := request.Offset
	if offset < 0 || offset > opened.Size() {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeNotFound, errors.New("offset is outside the file"))
	}
	length := request.Length
	if length <= 0 || length > workspacesession.MaxReadBytes {
		length = workspacesession.MaxReadBytes
	}
	remaining := opened.Size() - offset
	truncated := remaining > length
	if remaining < length {
		length = remaining
	}
	if _, err := handle.Seek(offset, io.SeekStart); err != nil {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, err)
	}
	buffer := make([]byte, length)
	read, err := io.ReadFull(handle, buffer)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return workspacesession.FilesContent{}, refuse(workspacesession.CodeForbidden, err)
	}
	buffer = buffer[:read]
	content = workspacesession.FilesContent{
		Path: relative, MIME: mimeFor(relative, buffer), Size: opened.Size(),
		Offset: offset, Truncated: truncated,
	}
	// Binary data is base64 in payload.data with payload.encoding "base64"
	// (section 18.2). Text is sent as text so a diff or a code view can use it
	// without a decode step.
	if utf8.Valid(buffer) && !bytes.ContainsRune(buffer, 0) {
		content.Data = string(buffer)
		return content, nil
	}
	content.Encoding = "base64"
	content.Data = base64.StdEncoding.EncodeToString(buffer)
	return content, nil
}

// resolve normalizes a requested path and applies the request-string rules.
//
// A directory symlink inside the worktree would otherwise let a request name a
// denied path under an alias ("alias/config" for ".git/config") that os.Root
// follows, so every directory component is resolved and the resolved path is
// held to the denylist as well.
func (s *Service) resolve(value string) (string, error) {
	relative, err := workspacesession.NormalizePath(value)
	if err != nil {
		return "", refuse(workspacesession.CodeNotFound, err)
	}
	resolved, err := s.resolveDirectories(relative)
	if err != nil {
		return "", err
	}
	if resolved != relative && s.deny.Denied(resolved) {
		return "", refuse(workspacesession.CodeDenied, errors.New(relative))
	}
	return relative, nil
}

// maxSymlinkHops bounds symlink resolution so a cycle is refused rather than
// followed forever.
const maxSymlinkHops = 40

// resolveDirectories follows every symlink among relative's directory
// components and reports the worktree-relative path they lead to. The final
// component is left as named: each operation lstats it and refuses or reports a
// symlink there on its own terms.
func (s *Service) resolveDirectories(relative string) (string, error) {
	if relative == "" {
		return "", nil
	}
	pending := strings.Split(relative, "/")
	resolved := []string{}
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", refuse(workspacesession.CodeForbidden, errors.New("a symbolic link leaves the worktree"))
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(slices.Clone(resolved), part), "/")
		if len(pending) == 0 {
			resolved = append(resolved, part)
			break
		}
		info, err := s.root.Lstat(candidate)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			resolved = append(resolved, part)
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return "", refuse(workspacesession.CodeForbidden, errors.New("too many symbolic links"))
		}
		target, err := s.root.Readlink(candidate)
		if err != nil {
			return "", translateOpenError(err)
		}
		target, err = s.worktreeRelativeTarget(target)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) || path.IsAbs(target) {
			resolved = resolved[:0]
			target = strings.TrimPrefix(filepath.ToSlash(target), "/")
		}
		pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
	}
	return strings.Join(resolved, "/"), nil
}

// worktreeRelativeTarget turns an absolute link target inside the worktree
// into a rooted worktree path ("/" plus the relative path) and refuses one
// outside it. A relative target is returned unchanged.
func (s *Service) worktreeRelativeTarget(target string) (string, error) {
	if !filepath.IsAbs(target) {
		return target, nil
	}
	inside, err := filepath.Rel(s.path, filepath.Clean(target))
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", refuse(workspacesession.CodeForbidden, errors.New("a symbolic link leaves the worktree"))
	}
	if inside == "." {
		return "/", nil
	}
	return "/" + filepath.ToSlash(inside), nil
}

// lstat stats a path without following a final symlink.
func (s *Service) lstat(relative string) (fs.FileInfo, error) {
	name := relative
	if name == "" {
		name = "."
	}
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, translateOpenError(err)
	}
	return info, nil
}

// openDirectory opens a directory for listing.
func (s *Service) openDirectory(relative string) (*os.File, fs.FileInfo, error) {
	if s.deny.Denied(relative) {
		return nil, nil, refuse(workspacesession.CodeDenied, errors.New(relative))
	}
	info, err := s.lstat(relative)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, refuse(workspacesession.CodeForbidden, errors.New("symbolic links are not followed"))
	}
	if !info.IsDir() {
		return nil, nil, refuse(workspacesession.CodeForbidden, errors.New("not a directory"))
	}
	name := relative
	if name == "" {
		name = "."
	}
	handle, err := s.root.OpenFile(name, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, nil, translateOpenError(err)
	}
	opened, err := handle.Stat()
	if err != nil {
		return nil, nil, errors.Join(refuse(workspacesession.CodeForbidden, err), handle.Close())
	}
	if !opened.IsDir() {
		return nil, nil, errors.Join(refuse(workspacesession.CodeForbidden, errors.New("not a directory")), handle.Close())
	}
	return handle, opened, nil
}

// requireSameDevice refuses anything on a filesystem other than the worktree's
// own. os.Root does not stop traversal of filesystem boundaries, and a bind
// mount inside a worktree is exactly how a reader would be shown something the
// worktree does not contain.
func (s *Service) requireSameDevice(info fs.FileInfo) error {
	if !s.hasDevice {
		return nil
	}
	device, ok := fileDevice(info)
	if !ok || device == s.device {
		return nil
	}
	return refuse(workspacesession.CodeForbidden, errors.New("mount point under the worktree"))
}

// translateOpenError maps a filesystem failure onto the channel's vocabulary.
// A permission failure answers forbidden and a missing path answers not_found;
// everything else answers forbidden, because a reader learning the difference
// between "this is a device" and "this is a socket" learns about the runner's
// machine rather than about the worktree.
func translateOpenError(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(workspacesession.CodeNotFound, err)
	case errors.Is(err, fs.ErrPermission):
		return refuse(workspacesession.CodeForbidden, err)
	default:
		return refuse(workspacesession.CodeForbidden, err)
	}
}

// mimeForPath guesses a media type from the extension alone.
func mimeForPath(relative string) string {
	if kind := mime.TypeByExtension(strings.ToLower(path.Ext(relative))); kind != "" {
		return kind
	}
	return "application/octet-stream"
}

// mimeFor decides a media type from the extension first and the content
// second. The extension is preferred because a source file's type is what the
// reader's highlighter needs and sniffing answers "text/plain" for all of them.
func mimeFor(relative string, content []byte) string {
	if kind := mime.TypeByExtension(strings.ToLower(path.Ext(relative))); kind != "" {
		return kind
	}
	if len(content) == 0 {
		return "text/plain; charset=utf-8"
	}
	head := content
	if len(head) > 512 {
		head = head[:512]
	}
	return http.DetectContentType(head)
}

// ignoredPaths reports which of the paths .gitignore covers, using git itself
// so the answer matches what the repository actually ignores rather than a
// re-implementation of its rules. A worktree with no git available reports
// nothing as ignored: showing a file that should have been hidden is a
// cosmetic failure, and hiding one that should be visible is not.
func (s *Service) ignoredPaths(ctx context.Context, paths []string) map[string]bool {
	ignored := map[string]bool{}
	if !s.hasGit || len(paths) == 0 {
		return ignored
	}
	checkCtx, cancel := context.WithTimeout(ctx, gitCheckTimeout)
	defer cancel()
	command := exec.CommandContext(checkCtx, "git", "check-ignore", "--stdin", "-z")
	// The worktree is the working directory rather than a -C argument: every
	// argument then being a literal is what makes it plain, here and to a
	// scanner, that nothing a reader sent reaches the command line.
	command.Dir = s.path
	command.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	output, err := command.Output()
	if err != nil {
		// git check-ignore exits 1 when nothing matched, which is not a
		// failure; anything else leaves the map empty.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return ignored
		}
	}
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			ignored[name] = true
		}
	}
	return ignored
}

// gitCheckTimeout bounds one ignore lookup. A listing that cannot be decorated
// in this long is served undecorated rather than not at all.
const gitCheckTimeout = 5 * time.Second

// DescribeLimits reports the caps this service enforces, for a runner log line
// that has to be honest about what a reader will hit.
func (s *Service) DescribeLimits() string {
	return "read " + strconv.Itoa(workspacesession.MaxReadBytes) + " bytes, " +
		strconv.Itoa(workspacesession.DirectoryPage) + " entries per page, " +
		strconv.Itoa(workspacesession.MaxReadsInFlight) + " reads in flight"
}
