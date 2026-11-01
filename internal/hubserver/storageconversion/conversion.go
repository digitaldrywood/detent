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
const MaxBytes int64 = 1 << 20

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

type Owner struct {
	repository Repository
	objects    attachment.DetailStore
	transform  Transform
}

func New(repository Repository, objects attachment.DetailStore, transform Transform) *Owner {
	return &Owner{repository: repository, objects: objects, transform: transform}
}

type Result struct {
	Next      Cursor
	Visited   int
	Converted int
	Bytes     int64
}

func (o *Owner) Batch(ctx context.Context, scope Scope, cursor Cursor, limit int) (Result, error) {
	if o.repository == nil || o.objects == nil || o.transform == nil || limit < 1 || limit > MaxRows {
		return Result{}, ErrInvalid
	}
	page, err := o.repository.ConversionPage(ctx, scope, cursor, limit)
	if err != nil {
		return Result{}, fmt.Errorf("read conversion page: %w", err)
	}
	result := Result{Next: page.Next}
	var failures error
	for _, candidate := range page.Items {
		if result.Visited > 0 && result.Bytes+candidate.Source.UncompressedSize > MaxBytes {
			result.Next.After = page.Items[result.Visited-1].Position
			return result, failures
		}
		result.Visited++
		result.Bytes += candidate.Source.UncompressedSize
		converted, targetBytes, err := o.convert(ctx, candidate)
		result.Bytes += targetBytes
		if err != nil {
			failures = errors.Join(failures, err)
		} else if converted {
			result.Converted++
		}
		if err := ctx.Err(); err != nil {
			return result, errors.Join(failures, err)
		}
		if result.Bytes >= MaxBytes {
			result.Next.After = candidate.Position
			return result, failures
		}
	}
	return result, failures
}

func (o *Owner) convert(ctx context.Context, candidate Candidate) (bool, int64, error) {
	organization := string(candidate.Scope.Organization)
	if candidate.Scope.Validate() != nil || candidate.FormatVersion != CurrentVersion-1 || candidate.Source.Validate(organization) != nil {
		return false, 0, ErrInvalid
	}
	raw, err := o.objects.ReadDetail(ctx, organization, candidate.Source)
	if err != nil {
		return false, 0, fmt.Errorf("read conversion source: %w", err)
	}
	if int64(len(raw)) != candidate.Source.UncompressedSize {
		return false, 0, artifact.ErrIntegrity
	}
	converted, err := o.transform(ctx, candidate, raw)
	if err != nil {
		return false, 0, fmt.Errorf("convert storage payload: %w", err)
	}
	targetBytes := int64(len(converted))
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(converted))
	if err != nil {
		return false, targetBytes, fmt.Errorf("prepare conversion object: %w", err)
	}
	stored, readErr := o.objects.ReadDetail(ctx, organization, ref)
	if readErr != nil || !bytes.Equal(stored, converted) {
		published, uploadErr := o.objects.StoreDetail(ctx, organization, converted)
		if uploadErr == nil && published != ref {
			return false, targetBytes, artifact.ErrIntegrity
		}
		stored, readErr = o.objects.ReadDetail(ctx, organization, ref)
		if readErr != nil {
			return false, targetBytes, fmt.Errorf("reconcile conversion upload: %w", errors.Join(uploadErr, readErr))
		}
		if !bytes.Equal(stored, converted) {
			return false, targetBytes, artifact.ErrIntegrity
		}
	}
	committed, err := o.repository.CommitConversion(ctx, candidate, ref)
	if err != nil {
		return false, targetBytes, fmt.Errorf("commit storage conversion: %w", err)
	}
	return committed, int64(len(converted)), nil
}
