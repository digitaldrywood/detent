package storageconversion

import "context"

type FamilyCursor struct {
	Family     string
	Turn       int64
	Positioned bool
	Keys       [4]string
	Sequence   int64
}

type Usage struct {
	Rows      int
	Bytes     int64
	Oversized int
}

type CursorUpdate int

const (
	Reserve CursorUpdate = iota
	Restore
	Record
)

type Turn struct {
	Visit   func(Candidate) bool
	Charge  func(int) error
	Expand  func(int) error
	Advance func(func(context.Context) error)
}

type Storage interface {
	Start(context.Context) ([]string, error)
	Next(context.Context, []string) (FamilyCursor, error)
	Update(context.Context, FamilyCursor, CursorUpdate, Usage) error
	Convert(context.Context, FamilyCursor, Turn) error
}
