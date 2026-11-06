package hubserver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
)

type sqliteConversionStorage struct {
	service *Service
	now     time.Time
}

type storageProjection struct {
	name    string
	convert func(context.Context) error
}

func (s sqliteConversionStorage) projections() []storageProjection {
	return []storageProjection{
		{"projection-content", s.service.convertPendingContent},
		{"projection-pages", s.service.convertIssuePageFacts},
		{"projection-activity", s.service.convertActivityProjections},
		{"projection-attempts", s.service.convertAttemptHotProjections},
		{"projection-leases", s.service.convertAttemptLeases},
		{"projection-health", s.service.convertHealthProjections},
	}
}

func (s sqliteConversionStorage) Start(ctx context.Context) ([]string, error) {
	projections := s.projections()
	names := make([]string, 0, len(projections)+len(storageConversionFamilies))
	for _, p := range projections {
		names = append(names, p.name)
	}
	for _, f := range storageConversionFamilies {
		names = append(names, f.name)
	}
	rows, err := s.service.database.reader.QueryContext(ctx, "SELECT family FROM storage_conversion_cursors WHERE "+storageConversionOwnerPredicate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existing := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		existing[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for _, name := range names {
		if !existing[name] {
			if _, err := s.service.database.db.ExecContext(ctx, `INSERT INTO storage_conversion_cursors(family) VALUES(?) ON CONFLICT DO NOTHING`, name); err != nil {
				return nil, err
			}
		}
	}

	return names, nil
}

func (s sqliteConversionStorage) Next(ctx context.Context, names []string) (storageconversion.FamilyCursor, error) {
	var cursor storageconversion.FamilyCursor
	err := s.service.database.reader.QueryRowContext(ctx, `SELECT family,turn,positioned,key_1,key_2,key_3,key_4,sequence FROM storage_conversion_cursors WHERE family IN (`+strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+`) ORDER BY turn,family LIMIT 1`, stringArguments(names)...).Scan(&cursor.Family, &cursor.Turn, &cursor.Positioned, &cursor.Keys[0], &cursor.Keys[1], &cursor.Keys[2], &cursor.Keys[3], &cursor.Sequence)
	return cursor, err
}

func (s sqliteConversionStorage) Update(ctx context.Context, cursor storageconversion.FamilyCursor, update storageconversion.CursorUpdate, usage storageconversion.Usage) error {
	var err error
	switch update {
	case storageconversion.Reserve:
		_, err = s.service.database.db.ExecContext(ctx, "UPDATE storage_conversion_cursors SET turn=(SELECT max(turn)+1 FROM storage_conversion_cursors WHERE "+storageConversionOwnerPredicate+") WHERE family=?", cursor.Family)
	case storageconversion.Restore:
		_, err = s.service.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET turn=? WHERE family=?`, cursor.Turn, cursor.Family)
	case storageconversion.Record:
		_, err = s.service.database.db.ExecContext(ctx, `UPDATE storage_conversion_cursors SET examined_rows=examined_rows+?,examined_bytes=examined_bytes+?,oversized_rows=oversized_rows+? WHERE family=?`, usage.Rows, usage.Bytes, usage.Oversized, cursor.Family)
	}
	return err
}

func (s sqliteConversionStorage) Convert(ctx context.Context, cursor storageconversion.FamilyCursor, _ storageconversion.Turn) error {
	projections := s.projections()
	index := slices.IndexFunc(projections, func(p storageProjection) bool { return p.name == cursor.Family })
	if index >= 0 {
		return projections[index].convert(ctx)
	}
	family := storageConversionFamilies[slices.IndexFunc(storageConversionFamilies, func(f storageConversionFamily) bool { return f.name == cursor.Family })]
	if (objectPayloadFamily(family.name) && (s.service.runDetailStore() != nil || s.service.storageArchiveContent() != nil)) || (!objectPayloadFamily(family.name) && s.service.storageArchiveContent() != nil) {
		return s.service.convertStorageFamilyTurn(ctx, family, storageFamilyCursor{family: cursor.Family, turn: cursor.Turn, positioned: cursor.Positioned, keys: cursor.Keys, sequence: cursor.Sequence}, s.now)
	}
	return nil
}
