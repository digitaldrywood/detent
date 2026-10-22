package attachment

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/artifact"
)

const MaxSectionBytes = 4 << 20

type ArchiveSection struct {
	SchemaVersion  int            `json:"schema_version"`
	OrganizationID string         `json:"organization_id"`
	ProjectID      string         `json:"project_id"`
	WorkItemID     string         `json:"work_item_id"`
	SectionID      string         `json:"section_id"`
	Tables         []ArchiveTable `json:"tables"`
}

type ArchiveTable struct {
	Name    string              `json:"name"`
	Columns []string            `json:"columns"`
	Rows    [][]json.RawMessage `json:"rows"`
}

func DecodeArchiveSection(organization string, ref ArchiveReference, data []byte) (ArchiveSection, error) {
	var section ArchiveSection
	if ref.Validate(organization) != nil || ref.SchemaVersion != 1 || ref.UncompressedSize > MaxSectionBytes || int64(len(data)) != ref.CompressedSize || artifact.Digest(data) != ref.SHA256 {
		return section, ErrInvalid
	}
	compressed := bytes.NewReader(data)
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		return section, ErrInvalid
	}
	reader.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil || int64(len(raw)) != ref.UncompressedSize || compressed.Len() != 0 {
		return section, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&section); err != nil || section.SchemaVersion != 1 || section.OrganizationID != organization {
		return ArchiveSection{}, ErrInvalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ArchiveSection{}, ErrInvalid
	}
	return section, nil
}
