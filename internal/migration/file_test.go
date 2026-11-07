package migration

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDuplicateKey(t *testing.T) {
	cases := []struct {
		json     string
		ptr, key string
	}{
		{json: `{"a": 1, "b": 2}`},
		{json: `{"a": {"x": 1}, "b": {"x": 2}}`},
		{json: `[{"a": 1}, {"a": 2}]`},
		{json: `{"a": 1, "a": 2}`, ptr: "/a", key: "a"},
		{json: `{"a": {"b": [1, {"c": 1, "d": [], "c": 2}]}}`, ptr: "/a/b/1/c", key: "c"},
		{json: `{"s": [[], [0, {"x/y": 1, "x/y": 2}]]}`, ptr: "/s/1/1/x~1y", key: "x/y"},
		// A key equal to a string value of the same object isn't a
		// duplicate.
		{json: `{"a": "b", "b": "a"}`},
	}
	for _, tc := range cases {
		t.Run(tc.json, func(t *testing.T) {
			ptr, key := duplicateKey([]byte(tc.json))
			if ptr != tc.ptr || key != tc.key {
				t.Errorf("duplicateKey = %q, %q; want %q, %q", ptr, key, tc.ptr, tc.key)
			}
		})
	}
}

// TestReadFile checks migration files are read only when they are regular
// files of at most MaxFileSize bytes.
func TestReadFile(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join("testdata", "valid", "2026-11-05-001-add-archived-flag.json"))
	if err != nil {
		t.Fatal(err)
	}
	const name = "2026-11-05-001-add-archived-flag.json"
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string) // creates dir/name
		want  error
	}{
		{name: "regular file", setup: func(t *testing.T, dir string) { write(t, filepath.Join(dir, name), valid) }},
		{name: "at the size limit", setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, name), append(valid, bytes.Repeat([]byte(" "), MaxFileSize-len(valid))...))
		}},
		{name: "over the size limit", want: ErrTooLarge, setup: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, name), append(valid, bytes.Repeat([]byte(" "), MaxFileSize-len(valid)+1)...))
		}},
		{name: "symbolic link", want: ErrNotRegular, setup: func(t *testing.T, dir string) {
			target := filepath.Join(t.TempDir(), "target.json")
			write(t, target, valid)
			if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
				t.Skip("symbolic links not supported:", err)
			}
		}},
		{name: "directory", want: ErrNotRegular, setup: func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			_, err := LoadFile(filepath.Join(dir, name))
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("LoadFile: %v, want %v", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), name) {
				t.Errorf("error doesn't name the file: %v", err)
			}
		})
	}
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
