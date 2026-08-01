package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectYAMLPaths walks t's fields (recursively through nested structs,
// slice elements, and map values), building the dotted key path
// docs/configuration.md is required to mention verbatim for each one.
// Duration is treated as a leaf despite being a struct: it has its own
// UnmarshalYAML/MarshalYAML and decodes from a plain string ("120s"), not
// from further nested keys. Slice-of-struct elements are recursed into
// under prefix+"[]" (e.g. "cron.jobs[].name"); map values are recursed into
// under prefix+".*" (e.g. "channels.telegram.groups.*.allow_from").
func collectYAMLPaths(t reflect.Type, prefix string, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		if t == reflect.TypeOf(Duration(0)) {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // unexported: not part of the YAML surface
				continue
			}
			tag := f.Tag.Get("yaml")
			if tag == "" || tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				continue
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			*out = append(*out, path)
			collectYAMLPaths(f.Type, path, out)
		}

	case reflect.Slice, reflect.Array:
		collectYAMLPaths(t.Elem(), prefix+"[]", out)

	case reflect.Map:
		collectYAMLPaths(t.Elem(), prefix+".*", out)
	}
}

// TestConfigFieldsAreDocumented asserts every YAML key path reachable from
// Config appears verbatim in docs/configuration.md, so a new field cannot
// ship without at least a documentation entry. It only checks presence,
// not accuracy - keeping the prose correct stays a review concern.
func TestConfigFieldsAreDocumented(t *testing.T) {
	docsPath := filepath.Join("..", "..", "docs", "configuration.md")
	data, err := os.ReadFile(docsPath)
	require.NoError(t, err, "docs/configuration.md must exist")
	content := string(data)

	var paths []string
	collectYAMLPaths(reflect.TypeOf(Config{}), "", &paths)
	require.NotEmpty(t, paths, "the field walker must find at least one key")

	var missing []string
	for _, p := range paths {
		if !strings.Contains(content, p) {
			missing = append(missing, p)
		}
	}
	assert.Empty(t, missing, "config keys not documented verbatim in docs/configuration.md: %v", missing)
}

// TestConfigFieldsAreDocumented_CatchesUndocumentedField proves the
// coverage mechanism above actually fails when a field is missing from the
// docs, using a throwaway type and a fixture string instead of leaving a
// real config field undocumented just to exercise the failure path.
func TestConfigFieldsAreDocumented_CatchesUndocumentedField(t *testing.T) {
	type fixture struct {
		Documented   string `yaml:"documented_key"`
		Undocumented string `yaml:"undocumented_key"`
	}
	fixtureDocs := "the docs mention documented_key but nothing else here"

	var paths []string
	collectYAMLPaths(reflect.TypeOf(fixture{}), "", &paths)

	var missing []string
	for _, p := range paths {
		if !strings.Contains(fixtureDocs, p) {
			missing = append(missing, p)
		}
	}
	assert.Equal(t, []string{"undocumented_key"}, missing, "the coverage mechanism must flag exactly the undocumented field, proving it fails on a real gap")
}
