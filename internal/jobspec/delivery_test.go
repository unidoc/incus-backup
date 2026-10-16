package jobspec

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/unidoc/incus-backup/internal/archive"
)

// The browser digests what it sent, the host what the server forwarded (the server's canonical
// copy): both must agree, and every parameter that changes the result must change the digest.
func TestRequestDigestIsCanonicalAndBindsEveryParameter(t *testing.T) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	pub := base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
	k2, _ := ecdh.X25519().GenerateKey(rand.Reader)
	pub2 := base64.StdEncoding.EncodeToString(k2.PublicKey().Bytes())
	cases := []struct{ typ, sent string }{
		{TypeFSExtract, `{"project":"default","object":"volume/docs","paths":["docs//2025/./a.txt","Q1/"],"recipient_public_key":"` + pub + `"}`},
		{TypeGuestFiles, `{"project":"default","object":"instance/web/root","paths":["/etc//nginx/","home/anna/../anna"],"partition":2,"recipient_public_key":"` + pub + `"}`},
	}
	for _, c := range cases {
		browser, err := RequestDigest(c.typ, []byte(c.sent))
		if err != nil {
			t.Fatal(err)
		}
		_, canon, err := Validate(c.typ, []byte(c.sent)) // what the server stores and forwards
		if err != nil {
			t.Fatal(err)
		}
		host, err := RequestDigest(c.typ, canon)
		if err != nil || !bytes.Equal(host, browser) {
			t.Fatalf("%s: the host's digest differs from the browser's: %v", c.typ, err)
		}
	}
	base := `{"project":"default","object":"volume/docs","snapshot":"20261007120000-00000001","paths":["a"],"recipient_public_key":"` + pub + `"}`
	d0, _ := RequestDigest(TypeFSExtract, []byte(base))
	for name, other := range map[string]string{
		"project":   `{"project":"prod","object":"volume/docs","snapshot":"20261007120000-00000001","paths":["a"],"recipient_public_key":"` + pub + `"}`,
		"object":    `{"project":"default","object":"volume/payroll","snapshot":"20261007120000-00000001","paths":["a"],"recipient_public_key":"` + pub + `"}`,
		"snapshot":  `{"project":"default","object":"volume/docs","snapshot":"20261007130000-00000002","paths":["a"],"recipient_public_key":"` + pub + `"}`,
		"latest":    `{"project":"default","object":"volume/docs","paths":["a"],"recipient_public_key":"` + pub + `"}`,
		"paths":     `{"project":"default","object":"volume/docs","snapshot":"20261007120000-00000001","paths":["a","b"],"recipient_public_key":"` + pub + `"}`,
		"recipient": `{"project":"default","object":"volume/docs","snapshot":"20261007120000-00000001","paths":["a"],"recipient_public_key":"` + pub2 + `"}`,
	} {
		d, err := RequestDigest(TypeFSExtract, []byte(other))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(d, d0) {
			t.Fatalf("changing %s does not change the digest", name)
		}
	}
	// the same parameters as another job type
	if d, _ := RequestDigest(TypeGuestFiles, []byte(`{"project":"default","object":"volume/docs","snapshot":"20261007120000-00000001","paths":["/a"],"recipient_public_key":"`+pub+`"}`)); bytes.Equal(d, d0) {
		t.Fatal("the type is not bound")
	}
	if _, err := RequestDigest(TypeFSExtract, []byte(`{"project":"default"}`)); err == nil {
		t.Fatal("invalid parameters digested")
	}
}

// guest_files may raise the recovered-entries cap, up to a hard ceiling; the default stays unchanged.
func TestGuestFilesMaxEntriesIsBounded(t *testing.T) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	pub := base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
	with := func(extra string) error {
		_, _, err := Validate(TypeGuestFiles, []byte(`{"project":"default","object":"instance/web/root","paths":["/etc"]`+extra+`,"recipient_public_key":"`+pub+`"}`))
		return err
	}
	for _, ok := range []string{``, `,"max_entries":0`, `,"max_entries":5000000`, fmt.Sprintf(`,"max_entries":%d`, MaxEntriesCeiling)} {
		if err := with(ok); err != nil {
			t.Errorf("%q must validate: %v", ok, err)
		}
	}
	for _, bad := range []string{`,"max_entries":-1`, fmt.Sprintf(`,"max_entries":%d`, MaxEntriesCeiling+1), `,"max_entries":"many"`} {
		if err := with(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q must be invalid, got %v", bad, err)
		}
	}
	if MaxEntriesCeiling != 10_000_000 {
		t.Errorf("the documented ceiling is 10,000,000, got %d", MaxEntriesCeiling)
	}
}

// jobspec cannot import internal/archive (it is compiled to WebAssembly), so it mirrors the limits.
func TestEntryLimitsMirrorArchive(t *testing.T) {
	if MaxEntriesCeiling != archive.MaxEntriesCeiling || DefaultMaxEntries != archive.DefaultMaxEntries {
		t.Errorf("jobspec limits %d/%d drifted from archive %d/%d",
			DefaultMaxEntries, MaxEntriesCeiling, archive.DefaultMaxEntries, archive.MaxEntriesCeiling)
	}
}
