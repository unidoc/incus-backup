package fscat

import (
	"strings"
	"testing"
)

// A hostile or damaged catalog cannot carry a name that is not one Linux path component: the
// decoder refuses "", ".", "..", "/" and NUL below the root, so nothing downstream (extraction,
// the browser's ZIP writer, the file browser) ever sees one. Names that ARE valid on Linux
// (backslashes, control characters, bidi overrides) decode and are neutralised where they leave
// the Linux world (web-crypto/src/zip.ts).
func TestDecodeRefusesNamesThatAreNotOneComponent(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "/", "nul\x00byte", "\x00"} {
		b, err := (&Tree{Root: &Entry{Kind: KDir, Children: []*Entry{{Kind: KFile, Name: bad}}}}).Bytes()
		if err != nil {
			continue // the encoder refused it already
		}
		if _, err := Decode(b); err == nil || !strings.Contains(err.Error(), "unsafe name") {
			t.Fatalf("name %q: %v", bad, err)
		}
		// also deeper in the tree
		b, _ = (&Tree{Root: &Entry{Kind: KDir, Children: []*Entry{{Kind: KDir, Name: "d", Children: []*Entry{{Kind: KFile, Name: bad}}}}}}).Bytes()
		if _, err := Decode(b); err == nil {
			t.Fatalf("nested name %q decoded", bad)
		}
	}
	for _, ok := range []string{`..\..\evil.bat`, "ctrl\x01\x1b[31m", "bidi‮txt.exe", "C:", " ", "-rf"} {
		b, err := (&Tree{Root: &Entry{Kind: KDir, Children: []*Entry{{Kind: KFile, Name: ok}}}}).Bytes()
		if err != nil {
			t.Fatal(err)
		}
		tr, err := Decode(b)
		if err != nil || tr.Root.Children[0].Name != ok {
			t.Fatalf("valid Linux name %q: %v", ok, err)
		}
	}
}
