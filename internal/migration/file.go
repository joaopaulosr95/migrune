package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Errors wrapped by LoadFile and LoadDir when a file can't be read or parsed.
var (
	// ErrNotRegular is a migration file that is a symbolic link or
	// anything else but a regular file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrTooLarge is a migration file over MaxFileSize bytes.
	ErrTooLarge = errors.New("file too large")
	// ErrBOM is a migration file that starts with a UTF-8 byte order
	// mark. It wraps ErrInvalidJSON too.
	ErrBOM = errors.New("UTF-8 byte order mark")
)

// MaxFileSize is the largest migration file migrune reads, in bytes.
const MaxFileSize = 1 << 20

// bom is the UTF-8 byte order mark.
var bom = []byte("\xef\xbb\xbf")

// ReadFile reads a migration file: a regular file, not a symbolic link, of
// at most MaxFileSize bytes. Otherwise it returns an error wrapping
// ErrNotRegular or ErrTooLarge.
func ReadFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := checkFileInfo(path, info); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file opened must be the one checked, not one swapped in since.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s: %w: it changed while being read", path, ErrNotRegular)
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxFileSize {
		return nil, fmt.Errorf("%s: %w: over %d bytes", path, ErrTooLarge, MaxFileSize)
	}
	return raw, nil
}

func checkFileInfo(path string, info os.FileInfo) error {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return fmt.Errorf("%s: %w: it is a symbolic link; migrune reads only regular files, so the checksum covers the file itself", path, ErrNotRegular)
	case !mode.IsRegular():
		return fmt.Errorf("%s: %w; migrune reads only regular files", path, ErrNotRegular)
	case info.Size() > MaxFileSize:
		return fmt.Errorf("%s: %w: %d bytes, over the %d-byte limit; split the migration", path, ErrTooLarge, info.Size(), MaxFileSize)
	}
	return nil
}

// duplicateKey returns the JSON pointer of the first key in raw that
// repeats a key of the same object, and that key, or "" when there is none.
// raw must be valid JSON.
func duplicateKey(raw []byte) (ptr, key string) {
	type frame struct {
		object  bool
		keys    map[string]bool
		key     string // the current key, in an object
		index   int    // the current index, in an array
		wantKey bool
	}
	var stack []*frame
	pointer := func() string {
		var sb strings.Builder
		for _, f := range stack {
			if f.object {
				sb.WriteString("/" + PointerToken(f.key))
			} else {
				sb.WriteString("/" + strconv.Itoa(f.index))
			}
		}
		return sb.String()
	}
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	// valueStart and valueEnd keep the current index and key in step.
	valueStart := func() {
		if t := top(); t != nil && !t.object {
			t.index++
		}
	}
	valueEnd := func() {
		if t := top(); t != nil && t.object {
			t.wantKey = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", ""
		}
		if t := top(); t != nil && t.object && t.wantKey {
			if k, ok := tok.(string); ok {
				t.key, t.wantKey = k, false
				if t.keys[k] {
					return pointer(), k
				}
				t.keys[k] = true
				continue
			}
		}
		switch tok {
		case json.Delim('{'):
			valueStart()
			stack = append(stack, &frame{object: true, keys: map[string]bool{}, wantKey: true})
		case json.Delim('['):
			valueStart()
			stack = append(stack, &frame{index: -1})
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			valueEnd()
		default:
			valueStart()
			valueEnd()
		}
	}
}
