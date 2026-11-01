package storageconversion

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const CurrentVersion int64 = 2
const MaxRows = 100

type Family string

const (
	Runs     Family = "runs"
	Versions Family = "versions"
	Evidence Family = "evidence"
)

var ErrInvalid = errors.New("invalid storage conversion")

type Scope struct {
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
	Family       Family
	AggregateID  string
}

func (s Scope) Validate() error {
	if s.Organization == "" || s.Project == "" || s.AggregateID == "" {
		return ErrInvalid
	}
	switch s.Family {
	case Runs, Versions, Evidence:
		return nil
	default:
		return ErrInvalid
	}
}

type Position struct {
	ID       string
	Sequence int64
}

type Cursor struct {
	Scope   Scope
	After   Position
	Through Position
}

type Candidate struct {
	Scope         Scope
	Position      Position
	FormatVersion int64
	Source        attachment.ArchiveReference
}

type Page struct {
	Items []Candidate
	Next  Cursor
}

type Progress struct {
	Rows  int64
	Bytes int64
}

type Repository interface {
	ConversionPage(context.Context, Scope, Cursor, int) (Page, error)
	CommitConversion(context.Context, Candidate, attachment.ArchiveReference) (bool, error)
}

type Transform func(context.Context, Candidate, []byte) ([]byte, error)

type Converter struct {
	repository Repository
	objects    attachment.DetailStore
	transform  Transform
}

func New(repository Repository, objects attachment.DetailStore, transform Transform) *Converter {
	return &Converter{repository: repository, objects: objects, transform: transform}
}

func (c *Converter) Convert(ctx context.Context, candidate Candidate, expand func(int) error) (bool, int64, error) {
	if c.repository == nil || c.objects == nil || c.transform == nil {
		return false, 0, ErrInvalid
	}
	organization := string(candidate.Scope.Organization)
	if candidate.Scope.Validate() != nil || candidate.FormatVersion != CurrentVersion-1 || candidate.Source.Validate(organization) != nil {
		return false, 0, ErrInvalid
	}
	raw, err := c.objects.ReadDetail(ctx, organization, candidate.Source)
	if err != nil {
		return false, 0, fmt.Errorf("read conversion source: %w", err)
	}
	if int64(len(raw)) != candidate.Source.UncompressedSize {
		return false, 0, artifact.ErrIntegrity
	}
	converted, err := c.transform(ctx, candidate, raw)
	if err != nil {
		return false, 0, fmt.Errorf("convert storage payload: %w", err)
	}
	targetBytes := int64(len(converted))
	if err := expand(len(raw) + len(converted)); err != nil {
		return false, 0, err
	}
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(converted))
	if err != nil {
		return false, targetBytes, fmt.Errorf("prepare conversion object: %w", err)
	}
	stored, readErr := c.objects.ReadDetail(ctx, organization, ref)
	if readErr != nil || !bytes.Equal(stored, converted) {
		published, uploadErr := c.objects.StoreDetail(ctx, organization, converted)
		if uploadErr == nil && published != ref {
			return false, targetBytes, artifact.ErrIntegrity
		}
		stored, readErr = c.objects.ReadDetail(ctx, organization, ref)
		if readErr != nil {
			return false, targetBytes, fmt.Errorf("reconcile conversion upload: %w", errors.Join(uploadErr, readErr))
		}
		if !bytes.Equal(stored, converted) {
			return false, targetBytes, artifact.ErrIntegrity
		}
	}
	committed, err := c.repository.CommitConversion(ctx, candidate, ref)
	if err != nil {
		return false, targetBytes, fmt.Errorf("commit storage conversion: %w", err)
	}
	return committed, int64(len(converted)), nil
}
