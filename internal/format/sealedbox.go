package format

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Sealed delivery stream, version 2 ("IBS2"): what a host produces FOR A BROWSER (files recovered
// from a VM disk or extracted from a filesystem snapshot). It is encrypted to a one-time X25519
// public key the browser created AND authenticated with a key derived from the repository master
// key, so
//   - the relaying server cannot read it (it has no X25519 private key), and
//   - the relaying server cannot forge, alter, truncate, reorder or substitute it: producing a
//     stream the browser accepts needs Keys.Delivery, which only holders of the repository key
//     have (every backup host of that key domain, and whoever has the recovery key). The server
//     knows the recipient public key (it is in the job parameters), which was enough to forge an
//     IBS1 stream; it is not enough here.
//   - a delivery made for another job, or for other parameters (another object, snapshot or
//     paths), does not authenticate either: both are bound into the key.
//
// Layout (docs/sealed-delivery.md is the normative description):
//
//	stream   = "IBS2" | ephemeral public (32) | data frame* | trailer frame
//	frame    = length (u32 big endian, 16 .. 1 MiB+16) | XChaCha20-Poly1305(k_stream, nonce, plaintext, aad)
//	nonce    = frame counter (u64 big endian, from 0, data frames and trailer alike), zero padded to 24 bytes
//	aad      = "IBS2" | 0x00 (data frame) or 0x01 (trailer)
//	data     = 1 .. 1 MiB plaintext bytes (never empty)
//	trailer  = total plaintext bytes (u64 BE) | data frames (u64 BE) | transcript (32)
//	transcript = SHA-256 over the header and every data frame exactly as sent (length | ciphertext)
//	k_auth   = HKDF-SHA256(ikm = Keys.Delivery, salt = "incus-backup/v1",
//	             info = "incus-backup sealed delivery v2 auth" | u16 len | job id | request digest (32) | recipient_pub | ephemeral_pub)
//	k_stream = HKDF-SHA256(ikm = X25519(ephemeral, recipient) | k_auth, salt = ephemeral_pub | recipient_pub,
//	             info = "incus-backup sealed stream v2")
//
// The request digest is jobspec.RequestDigest of the job parameters (computed by the browser from
// what it asked for and by the host from what it received). The version-1 format ("IBS1", only
// the X25519 part) is refused: it was not authenticated against the server.
const (
	sealMagic     = "IBS2"
	sealMagicV1   = "IBS1"
	sealFrameSize = 1 << 20
	// SealHeaderSize is the size of the stream header ("IBS2" | ephemeral public key).
	SealHeaderSize = 4 + 32
	// SealMaxFrame is the largest valid frame body (ciphertext of a full data frame).
	SealMaxFrame   = sealFrameSize + chacha20poly1305.Overhead
	sealTrailerLen = 8 + 8 + sha256.Size
	sealJobIDMax   = 256
)

// SealBinding names what a delivery answers. Both ends must agree on it exactly.
type SealBinding struct {
	JobID   string // the job id (as the server issued it)
	Request []byte // jobspec.RequestDigest of the job parameters (32 bytes)
}

func (b SealBinding) check() error {
	if b.JobID == "" || len(b.JobID) > sealJobIDMax {
		return errors.New("sealed stream: a job id (1..256 bytes) is required")
	}
	if len(b.Request) != sha256.Size {
		return errors.New("sealed stream: the request digest must be 32 bytes")
	}
	return nil
}

// ErrSealV1 is returned for a version-1 sealed stream, which is no longer accepted.
var ErrSealV1 = errors.New("sealed stream: this is an IBS1 delivery, which is not authenticated against the server and is refused; update incus-backup on the host and run the job again")

func sealStreamKey(k *Keys, shared, eph, recipient []byte, b SealBinding) (cipher.AEAD, error) {
	if k == nil {
		return nil, errors.New("sealed stream: the repository key is required")
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	info := []byte("incus-backup sealed delivery v2 auth")
	info = binary.BigEndian.AppendUint16(info, uint16(len(b.JobID)))
	info = append(info, b.JobID...)
	info = append(info, b.Request...)
	info = append(info, recipient...)
	info = append(info, eph...)
	kauth := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, k.Delivery[:], []byte(hkdfSalt), info), kauth); err != nil {
		return nil, err
	}
	ikm := append(append(make([]byte, 0, len(shared)+KeySize), shared...), kauth...)
	salt := append(append([]byte{}, eph...), recipient...)
	ks := make([]byte, chacha20poly1305.KeySize)
	_, err := io.ReadFull(hkdf.New(sha256.New, ikm, salt, []byte("incus-backup sealed stream v2")), ks)
	wipeBytes(kauth)
	wipeBytes(ikm)
	if err != nil {
		return nil, err
	}
	a, err := chacha20poly1305.NewX(ks)
	wipeBytes(ks)
	return a, err
}

func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func sealNonce(n uint64) []byte {
	b := make([]byte, chacha20poly1305.NonceSizeX)
	binary.BigEndian.PutUint64(b, n)
	return b
}

func sealAAD(trailer bool) []byte {
	if trailer {
		return []byte(sealMagic + "\x01")
	}
	return []byte(sealMagic + "\x00")
}

// NewSealWriter returns a writer that seals to recipientPub (32 bytes) and authenticates with k
// for the delivery b. Close MUST be called: it writes the trailer; a stream without it is refused.
func NewSealWriter(w io.Writer, k *Keys, recipientPub []byte, b SealBinding) (io.WriteCloser, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return newSealWriter(w, k, recipientPub, b, eph)
}

func newSealWriter(w io.Writer, k *Keys, recipientPub []byte, b SealBinding, eph *ecdh.PrivateKey) (io.WriteCloser, error) {
	rp, err := ecdh.X25519().NewPublicKey(recipientPub)
	if err != nil {
		return nil, err
	}
	shared, err := eph.ECDH(rp)
	if err != nil {
		return nil, err
	}
	aead, err := sealStreamKey(k, shared, eph.PublicKey().Bytes(), recipientPub, b)
	wipeBytes(shared)
	if err != nil {
		return nil, err
	}
	hdr := append([]byte(sealMagic), eph.PublicKey().Bytes()...)
	tr := sha256.New()
	tr.Write(hdr)
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &sealWriter{w: w, aead: aead, tr: tr, buf: make([]byte, 0, sealFrameSize)}, nil
}

type sealWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	tr     hash.Hash
	buf    []byte
	n      uint64 // frames written (data and trailer share the counter)
	total  uint64 // plaintext bytes
	closed bool
	err    error
}

func (s *sealWriter) put(pt []byte, trailer bool) error {
	ct := s.aead.Seal(nil, sealNonce(s.n), pt, sealAAD(trailer))
	s.n++
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
	if !trailer {
		s.tr.Write(hdr[:])
		s.tr.Write(ct)
	}
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := s.w.Write(ct)
	return err
}

func (s *sealWriter) flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	s.total += uint64(len(s.buf))
	err := s.put(s.buf, false)
	wipeBytes(s.buf)
	s.buf = s.buf[:0]
	return err
}

func (s *sealWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("sealed stream is closed")
	}
	if s.err != nil {
		return 0, s.err
	}
	total := len(p)
	for len(p) > 0 {
		if len(s.buf) == sealFrameSize {
			if s.err = s.flush(); s.err != nil {
				return total - len(p), s.err
			}
		}
		n := min(sealFrameSize-len(s.buf), len(p))
		s.buf = append(s.buf, p[:n]...)
		p = p[n:]
	}
	return total, nil
}

// Close writes the last data frame and the trailer.
func (s *sealWriter) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.err != nil {
		return s.err
	}
	if s.err = s.flush(); s.err != nil {
		return s.err
	}
	t := make([]byte, 0, sealTrailerLen)
	t = binary.BigEndian.AppendUint64(t, s.total)
	t = binary.BigEndian.AppendUint64(t, s.n)
	t = s.tr.Sum(t)
	s.err = s.put(t, true)
	return s.err
}

// SealOpener verifies and decrypts a sealed delivery frame by frame (push-driven; NewSealReader
// is the pull form). Every frame authenticates under a key that needs both the recipient's X25519
// private key and the repository key; the trailer authenticates the total length, the frame count
// and a hash of everything before it. Done reports true only after the trailer verified; anything
// a caller received before that is not yet known to be complete.
type SealOpener struct {
	k      *Keys
	priv   *ecdh.PrivateKey
	b      SealBinding
	aead   cipher.AEAD
	tr     hash.Hash
	n      uint64
	total  uint64
	done   bool
	failed error
}

// NewSealOpener prepares to open a delivery addressed to recipientPriv (32 bytes) for binding b.
func NewSealOpener(k *Keys, recipientPriv []byte, b SealBinding) (*SealOpener, error) {
	if k == nil {
		return nil, errors.New("sealed stream: the repository key is required")
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	sk, err := ecdh.X25519().NewPrivateKey(recipientPriv)
	if err != nil {
		return nil, errors.New("sealed stream: the private key is not a valid X25519 key")
	}
	return NewSealOpenerKey(k, sk, b)
}

// NewSealOpenerKey is NewSealOpener for a key that is already an *ecdh.PrivateKey (X25519).
func NewSealOpenerKey(k *Keys, recipient *ecdh.PrivateKey, b SealBinding) (*SealOpener, error) {
	if k == nil {
		return nil, errors.New("sealed stream: the repository key is required")
	}
	if err := b.check(); err != nil {
		return nil, err
	}
	if recipient == nil || recipient.Curve() != ecdh.X25519() {
		return nil, errors.New("sealed stream: the private key is not a valid X25519 key")
	}
	b.Request = append([]byte(nil), b.Request...)
	return &SealOpener{k: k, priv: recipient, b: b}, nil
}

func (o *SealOpener) fail(err error) error {
	if o.failed == nil {
		o.failed = err
	}
	return o.failed
}

// Header consumes the SealHeaderSize-byte stream header.
func (o *SealOpener) Header(h []byte) error {
	if o.failed != nil {
		return o.failed
	}
	if o.aead != nil {
		return o.fail(errors.New("sealed stream: header given twice"))
	}
	if len(h) >= 4 && string(h[:4]) == sealMagicV1 {
		return o.fail(ErrSealV1)
	}
	if len(h) != SealHeaderSize {
		return o.fail(errors.New("sealed stream is truncated (incomplete header)"))
	}
	if string(h[:4]) != sealMagic {
		return o.fail(errors.New("not a sealed stream (bad magic)"))
	}
	eph, err := ecdh.X25519().NewPublicKey(h[4:])
	if err != nil {
		return o.fail(errors.New("sealed stream: bad ephemeral key"))
	}
	shared, err := o.priv.ECDH(eph)
	if err != nil {
		return o.fail(errors.New("sealed stream: a frame failed authentication (low-order ephemeral key)"))
	}
	aead, err := sealStreamKey(o.k, shared, h[4:], o.priv.PublicKey().Bytes(), o.b)
	wipeBytes(shared)
	if err != nil {
		return o.fail(err)
	}
	o.aead = aead
	o.tr = sha256.New()
	o.tr.Write(h)
	return nil
}

// FrameLen validates the 4-byte length prefix of the next frame and returns the body length.
func FrameLen(hdr []byte) (int, error) {
	n := binary.BigEndian.Uint32(hdr)
	if n < chacha20poly1305.Overhead || n > SealMaxFrame {
		return 0, errors.New("sealed stream: bad frame length")
	}
	return int(n), nil
}

// Frame verifies one frame body (without its length prefix) and returns its plaintext; trailer is
// true for the authenticated trailer (no plaintext, the stream is then complete).
func (o *SealOpener) Frame(body []byte) (pt []byte, trailer bool, err error) {
	if o.failed != nil {
		return nil, false, o.failed
	}
	if o.aead == nil {
		return nil, false, o.fail(errors.New("sealed stream: frame before the header"))
	}
	if o.done {
		return nil, false, o.fail(errors.New("sealed stream: data after the final frame"))
	}
	if len(body) < chacha20poly1305.Overhead || len(body) > SealMaxFrame {
		return nil, false, o.fail(errors.New("sealed stream: bad frame length"))
	}
	nonce := sealNonce(o.n)
	if pt, err := o.aead.Open(nil, nonce, body, sealAAD(false)); err == nil {
		if len(pt) == 0 {
			return nil, false, o.fail(errors.New("sealed stream: empty data frame"))
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(body)))
		o.tr.Write(l[:])
		o.tr.Write(body)
		o.n++
		o.total += uint64(len(pt))
		return pt, false, nil
	}
	t, err := o.aead.Open(nil, nonce, body, sealAAD(true))
	if err != nil {
		return nil, false, o.fail(errors.New("sealed stream: a frame failed authentication (wrong key, another job's delivery, or altered data)"))
	}
	if len(t) != sealTrailerLen {
		return nil, false, o.fail(errors.New("sealed stream: bad trailer"))
	}
	if binary.BigEndian.Uint64(t[:8]) != o.total || binary.BigEndian.Uint64(t[8:16]) != o.n || !bytes.Equal(t[16:], o.tr.Sum(nil)) {
		return nil, false, o.fail(errors.New("sealed stream: the trailer does not match the data (frames dropped, reordered or altered)"))
	}
	o.n++
	o.done = true
	return nil, true, nil
}

// Done reports whether the trailer verified: only then is the stream complete and authentic.
func (o *SealOpener) Done() bool { return o.done && o.failed == nil }

// Total returns the plaintext bytes verified so far.
func (o *SealOpener) Total() uint64 { return o.total }

// End returns nil only for a complete, authenticated stream.
func (o *SealOpener) End() error {
	if o.failed != nil {
		return o.failed
	}
	if o.aead == nil {
		return errors.New("sealed stream is truncated (incomplete header)")
	}
	if !o.done {
		return errors.New("sealed stream is truncated (no final frame)")
	}
	return nil
}

// NewSealReader decrypts a sealed delivery addressed to recipientPriv, authenticated with k for
// binding b. Data is returned as frames verify; io.EOF comes only after the trailer verified, so a
// consumer must treat what it read as incomplete until it sees io.EOF (a truncated, reordered,
// substituted or altered stream yields an error instead).
func NewSealReader(r io.Reader, k *Keys, recipientPriv []byte, b SealBinding) (io.Reader, error) {
	o, err := NewSealOpener(k, recipientPriv, b)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, SealHeaderSize)
	n, err := io.ReadFull(r, hdr)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("sealed stream: %w", err)
	}
	if err := o.Header(hdr[:n]); err != nil {
		return nil, err
	}
	return &sealReader{r: r, o: o}, nil
}

type sealReader struct {
	r   io.Reader
	o   *SealOpener
	cur []byte
	eof bool
}

func (s *sealReader) Read(p []byte) (int, error) {
	for len(s.cur) == 0 {
		if s.eof {
			return 0, io.EOF
		}
		var hdr [4]byte
		if _, err := io.ReadFull(s.r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, s.o.fail(errors.New("sealed stream is truncated (no final frame)"))
			}
			return 0, err
		}
		n, err := FrameLen(hdr[:])
		if err != nil {
			return 0, s.o.fail(err)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(s.r, body); err != nil {
			return 0, s.o.fail(errors.New("sealed stream is truncated"))
		}
		pt, trailer, err := s.o.Frame(body)
		if err != nil {
			return 0, err
		}
		if trailer {
			// nothing may follow the trailer
			var one [1]byte
			if k, _ := io.ReadFull(s.r, one[:]); k > 0 {
				return 0, s.o.fail(errors.New("sealed stream: data after the final frame"))
			}
			s.eof = true
			return 0, io.EOF
		}
		s.cur = pt
	}
	n := copy(p, s.cur)
	s.cur = s.cur[n:]
	return n, nil
}
