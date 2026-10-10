package format

import (
	"bytes"
	"strings"
	"testing"
)

func TestEscrowRoundTripAndEveryFailureMode(t *testing.T) {
	master, _ := GenerateMaster()
	priv, pub, err := GenerateEscrowKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(priv, "IBE1-") || !strings.HasPrefix(pub, "IBP1-") {
		t.Fatalf("%s %s", priv, pub)
	}
	if p, err := EscrowPublicOf(priv); err != nil || p != pub {
		t.Fatalf("public of private: %v %s", err, p)
	}
	w, err := WrapMasterKey(master, pub)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(w, master) {
		t.Fatal("the master key appears in the wrapped blob")
	}
	got, dom, err := UnwrapMasterKey(w, priv)
	if err != nil || !bytes.Equal(got, master) {
		t.Fatalf("round trip: %v", err)
	}
	k, _ := DeriveKeys(master)
	if dom != k.DomainID {
		t.Fatal("wrong domain")
	}
	// a different escrow key cannot open it
	priv2, _, _ := GenerateEscrowKeyPair()
	if _, _, err := UnwrapMasterKey(w, priv2); err == nil {
		t.Fatal("a different private key opened the blob")
	}
	// every byte of the blob is authenticated: flip each region
	for _, i := range []int{0, 5, 20, 40, 70, len(w) - 1} {
		bad := append([]byte(nil), w...)
		bad[i] ^= 1
		if _, _, err := UnwrapMasterKey(bad, priv); err == nil {
			t.Fatalf("a flipped byte at %d was accepted", i)
		}
	}
	// a swapped domain id is caught (it is part of the AAD)
	swapped := append([]byte(nil), w...)
	copy(swapped[4:], "0000000000000000")
	if _, _, err := UnwrapMasterKey(swapped, priv); err == nil {
		t.Fatal("a swapped domain id was accepted")
	}
	// typos in the printable keys are caught by their checksum
	// (not the last character: it carries unused base32 bits that decode to the same key)
	i := len(priv) - 6
	typo := priv[:i] + map[bool]string{true: "A", false: "B"}[priv[i] != 'A'] + priv[i+1:]
	if _, _, err := UnwrapMasterKey(w, typo); err == nil {
		t.Fatal("a mistyped private key was accepted")
	}
	if _, err := WrapMasterKey(master, priv); err == nil {
		t.Fatal("wrapping to a private key (wrong kind of key) was accepted")
	}
	// two wraps of the same key differ (fresh ephemeral key and nonce)
	w2, _ := WrapMasterKey(master, pub)
	if bytes.Equal(w, w2) {
		t.Fatal("wrapping is deterministic")
	}
	// the recovery key still works independently
	rec, _ := EncodeRecoveryKey(master)
	if m, err := DecodeRecoveryKey(rec); err != nil || !bytes.Equal(m, master) {
		t.Fatal("recovery key broken")
	}
}
