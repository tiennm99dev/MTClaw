package store

import (
	"database/sql"
	"time"
)

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting one scan
// helper serve both a single-row Get and a multi-row List.
type rowScanner interface {
	Scan(dest ...any) error
}

// toMillis converts a Go time to unix millis for storage. A zero time
// stores as 0.
func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromMillis is the inverse of toMillis; 0 decodes back to the zero
// time.Time.
func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// toNullMillis converts an optional Go time (nil or zero means absent) to
// a nullable integer column.
func toNullMillis(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

// fromNullMillis is the inverse of toNullMillis.
func fromNullMillis(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

// toNullInt converts an optional *int to a nullable integer column.
func toNullInt(v *int) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*v), Valid: true}
}

// fromNullInt is the inverse of toNullInt.
func fromNullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

// toNullInt64 converts an optional *int64 to a nullable integer column.
func toNullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// fromNullInt64 is the inverse of toNullInt64.
func fromNullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	i := v.Int64
	return &i
}

// boolToInt converts a Go bool to the 0/1 integer it stores as.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
