package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateKeyAcceptsSHA256(t *testing.T) {
	good := []string{
		strings.Repeat("0", 64),
		strings.Repeat("f", 64),
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // sha256("")
	}
	for _, k := range good {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
}

// TestValidateKeyRejects is the falsifier for "a malformed key cannot reach the
// filesystem". Every entry here is a real thing a client or an attacker sends.
func TestValidateKeyRejects(t *testing.T) {
	bad := map[string]string{
		"empty":            "",
		"too short":        strings.Repeat("a", 63),
		"too long":         strings.Repeat("a", 65),
		"uppercase":        strings.Repeat("A", 64),
		"mixed case":       "E3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"non-hex letter":   strings.Repeat("g", 64),
		"path traversal":   "../" + strings.Repeat("a", 61),
		"dot segments":     strings.Repeat(".", 64),
		"slash":            strings.Repeat("a", 32) + "/" + strings.Repeat("a", 31),
		"null byte":        strings.Repeat("a", 63) + "\x00",
		"newline":          strings.Repeat("a", 63) + "\n",
		"space":            strings.Repeat("a", 63) + " ",
		"unicode":          strings.Repeat("a", 63) + "é",
		"absolute path":    "/etc/passwd",
		"windows sep":      strings.Repeat("a", 32) + "\\" + strings.Repeat("a", 31),
		"url encoded dots": "%2e%2e%2f" + strings.Repeat("a", 55),
	}
	for name, k := range bad {
		err := ValidateKey(k)
		if err == nil {
			t.Errorf("%s: ValidateKey(%q) = nil, want error", name, k)
			continue
		}
		if !errors.Is(err, ErrBadKey) {
			t.Errorf("%s: error is not ErrBadKey: %v", name, err)
		}
	}
}

func TestParseNamespace(t *testing.T) {
	for _, in := range []string{"cas", "CAS", "Cas"} {
		ns, err := ParseNamespace(in)
		if err != nil || ns != NamespaceCAS {
			t.Errorf("ParseNamespace(%q) = %v, %v", in, ns, err)
		}
	}
	for _, in := range []string{"ac", "AC"} {
		ns, err := ParseNamespace(in)
		if err != nil || ns != NamespaceAC {
			t.Errorf("ParseNamespace(%q) = %v, %v", in, ns, err)
		}
	}
	for _, in := range []string{"", "casx", "blobs", "../cas", "cas/"} {
		if _, err := ParseNamespace(in); !errors.Is(err, ErrBadNamespace) {
			t.Errorf("ParseNamespace(%q) = %v, want ErrBadNamespace", in, err)
		}
	}
}

func TestNamespaceContentAddressed(t *testing.T) {
	if !NamespaceCAS.ContentAddressed() {
		t.Error("CAS is not content-addressed")
	}
	// The whole AC design rests on this being false. If it flips, the write
	// path starts rejecting legitimate ActionResult updates.
	if NamespaceAC.ContentAddressed() {
		t.Error("AC is content-addressed; action results must be overwritable")
	}
}

// TestObjectPathStaysUnderRoot is the falsifier for path traversal. Keys are
// validated before they reach ObjectPath, but this asserts the second line of
// defence independently.
func TestObjectPathStaysUnderRoot(t *testing.T) {
	s := newTestStore(t, Options{})
	key := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	dir, path := s.ObjectPath(NamespaceCAS, key)
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, s.Root()+string(filepath.Separator)) {
		t.Fatalf("object path %q escapes root %q", clean, s.Root())
	}
	if filepath.Dir(clean) != dir {
		t.Errorf("dir %q is not the parent of %q", dir, clean)
	}
	// Two levels of one hex byte: <root>/cas/e3/b0/<key>
	rel, err := filepath.Rel(s.Root(), clean)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("cas", "e3", "b0", key)
	if rel != want {
		t.Errorf("relative path = %q, want %q", rel, want)
	}
}

func TestObjectPathSeparatesNamespaces(t *testing.T) {
	s := newTestStore(t, Options{})
	key := strings.Repeat("ab", 32)
	_, casPath := s.ObjectPath(NamespaceCAS, key)
	_, acPath := s.ObjectPath(NamespaceAC, key)
	if casPath == acPath {
		t.Fatal("CAS and AC share a path; an action result would overwrite an object")
	}
}
