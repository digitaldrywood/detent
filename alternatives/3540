package postgres

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
)

const (
	MigrationRoleName = "detent_migration_v2"
	RuntimeRoleName   = "detent_runtime_v2"
	PlatformRoleName  = "detent_platform_v2"
)

type roleMigrationFS struct {
	fs.FS
}

func RoleMigrations(source fs.FS) fs.FS {
	if source == nil {
		return nil
	}
	return roleMigrationFS{FS: source}
}

func (source roleMigrationFS) Open(name string) (fs.File, error) {
	file, err := source.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if path.Ext(name) != ".sql" {
		return file, nil
	}

	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	contents = rewriteMigrationRoles(contents)
	return &roleMigrationFile{
		Reader: bytes.NewReader(contents),
		info:   roleMigrationInfo{FileInfo: info, size: int64(len(contents))},
	}, nil
}

func rewriteMigrationRoles(contents []byte) []byte {
	var result strings.Builder
	result.Grow(len(contents))
	for offset := 0; offset < len(contents); {
		if !roleIdentifierStart(contents[offset]) {
			result.WriteByte(contents[offset])
			offset++
			continue
		}
		end := offset + 1
		for end < len(contents) && roleIdentifierPart(contents[end]) {
			end++
		}
		identifier := string(contents[offset:end])
		if replacement, ok := migrationRoleReplacement(identifier); ok {
			result.WriteString(replacement)
		} else {
			result.Write(contents[offset:end])
		}
		offset = end
	}
	return []byte(result.String())
}

func migrationRoleReplacement(identifier string) (string, bool) {
	switch identifier {
	case "detent_migration":
		return MigrationRoleName, true
	case "detent_runtime":
		return RuntimeRoleName, true
	case "detent_platform":
		return PlatformRoleName, true
	default:
		return "", false
	}
}

func roleIdentifierStart(character byte) bool {
	return character >= 0x80 || character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func roleIdentifierPart(character byte) bool {
	return roleIdentifierStart(character) || character >= '0' && character <= '9' || character == '$'
}

type roleMigrationFile struct {
	*bytes.Reader
	info fs.FileInfo
}

type roleMigrationInfo struct {
	fs.FileInfo
	size int64
}

func (info roleMigrationInfo) Size() int64 {
	return info.size
}

func (file *roleMigrationFile) Stat() (fs.FileInfo, error) {
	return file.info, nil
}

func (file *roleMigrationFile) Close() error {
	return nil
}
