package store

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadMigrations_RejectsDuplicateVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/003_a.sql": {Data: []byte("SELECT 1;")},
		"migrations/003_b.sql": {Data: []byte("SELECT 2;")},
	}
	_, err := readMigrations(fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "share version 3")
}

func TestReadMigrations_AcceptsDistinctVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/002_b.sql": {Data: []byte("SELECT 2;")},
		"migrations/001_a.sql": {Data: []byte("SELECT 1;")},
	}
	got, err := readMigrations(fsys)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].version)
	assert.Equal(t, 2, got[1].version)
}
