package action

import (
	"strings"
	"testing"
)

// TestExtension: a saved attachment takes the first usable extension of its
// file names (lower-cased; at most 8 characters with the dot, no path
// separator), else one for its MIME type, else ".bin".
func TestExtension(t *testing.T) {
	for _, c := range []struct {
		mime  string
		names []string
		want  string
	}{
		{"application/pdf", []string{"Flyer.PDF"}, ".pdf"},
		{"image/jpeg", []string{"", "photo.JpG"}, ".jpg"},
		{"application/x-unknown", []string{"notes", "notes.txt"}, ".txt"},
		{"application/x-unknown", []string{"archive.tar.gz"}, ".gz"},
		{"application/x-unknown", []string{"x.verylongext"}, ".bin"},
		{"application/x-unknown", []string{`a.b\c`}, ".bin"},
		{"application/x-unknown", []string{"../../etc/passwd"}, ".bin"},
		{"", nil, ".bin"},
		{"not a mime type", []string{""}, ".bin"},
	} {
		if got := extension(c.mime, c.names...); got != c.want {
			t.Errorf("extension(%q, %q) = %q, want %q", c.mime, c.names, got, c.want)
		}
	}
	// With no usable name the MIME type decides (the system's table may list
	// several extensions; any is one of its own).
	if got := extension("image/png", "x.verylongext"); got == ".bin" || !strings.HasPrefix(got, ".") || strings.ContainsAny(got, `/\`) {
		t.Errorf("image/png: %q", got)
	}
}
