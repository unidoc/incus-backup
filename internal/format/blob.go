package format

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"
)

// Chunk blob layout (all integers big endian), format version 1:
//
//	magic    4  "IBCH"
//	version  1  0x01
//	flags    1  bit0: plaintext was zstd compressed before encryption
//	reserved 2  0x0000
//	nonce   24  random
//	ct       n  XChaCha20-Poly1305(k_enc, nonce, [zstd](plaintext), aad)
//	trailer 32  SHA-256 over every preceding byte (verifiable without any key)
//
// aad = magic | version | flags | reserved | chunk id (32 bytes).
const (
	blobMagic    = "IBCH"
	blobVersion  = 1
	blobHeader   = 4 + 1 + 1 + 2
	flagZstd     = 1
	trailerSize  = sha256.Size
	blobOverhead = blobHeader + NonceSize + chacha20poly1305.Overhead + trailerSize
)

// MaxChunkSize bounds the decompressed size accepted by Decode (zip-bomb guard).
const MaxChunkSize = 64 << 20

// zstd encoders and decoders. A klauspost Encoder/Decoder built with concurrency 1 serialises
// every EncodeAll/DecodeAll on it, so one shared instance would make the client's worker pool
// compress one chunk at a time. Instead every call takes its own instance from a free list; all
// are built with the SAME options, so the bytes produced are identical to the single shared
// encoder this replaced (pinned by TestCompressedEncodingIsPinned).
func newZEnc() *zstd.Encoder {
	e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	return e
}

func newZDec() *zstd.Decoder {
	d, _ := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxChunkSize), zstd.WithDecoderConcurrency(1))
	return d
}

// zFree bounds the idle instances kept for reuse (more may exist while more goroutines encode).
var zFree = runtime.GOMAXPROCS(0) * 2

var (
	encFree = make(chan *zstd.Encoder, zFree)
	decFree = make(chan *zstd.Decoder, zFree)
	// scratch buffers for the compressed form, which is copied into the blob by Seal
	scratch = sync.Pool{New: func() any { return new([]byte) }}
)

func getZEnc() *zstd.Encoder {
	select {
	case e := <-encFree:
		return e
	default:
		return newZEnc()
	}
}

func putZEnc(e *zstd.Encoder) {
	select {
	case encFree <- e:
	default:
	}
}

func getZDec() *zstd.Decoder {
	select {
	case d := <-decFree:
		return d
	default:
		return newZDec()
	}
}

func putZDec(d *zstd.Decoder) {
	select {
	case decFree <- d:
	default:
	}
}

// compress returns the zstd frame of src in a new slice.
func compress(src []byte) []byte {
	e := getZEnc()
	defer putZEnc(e)
	return e.EncodeAll(src, nil)
}

// EncodeOpts tune blob encoding.
type EncodeOpts struct {
	// NoCompress disables zstd.
	NoCompress bool
	// Nonce overrides the random nonce (tests only).
	Nonce []byte
}

func blobAAD(flags byte, id ID) []byte {
	aad := make([]byte, 0, blobHeader+IDSize)
	aad = append(aad, blobMagic...)
	aad = append(aad, blobVersion, flags, 0, 0)
	aad = append(aad, id[:]...)
	return aad
}

// EncodeChunk returns the keyed chunk id and the encrypted blob.
func (k *Keys) EncodeChunk(plaintext []byte, o EncodeOpts) (ID, []byte, error) {
	id := k.ChunkID(plaintext)
	body := plaintext
	var flags byte
	if !o.NoCompress {
		e := getZEnc()
		bp := scratch.Get().(*[]byte)
		if need := e.MaxEncodedSize(len(plaintext)); cap(*bp) < need {
			*bp = make([]byte, 0, need)
		}
		c := e.EncodeAll(plaintext, (*bp)[:0])
		putZEnc(e)
		defer func() { *bp = c[:0]; scratch.Put(bp) }()
		if len(c) < len(plaintext) {
			body, flags = c, flagZstd
		}
	}
	nonce := o.Nonce
	if nonce == nil {
		nonce = make([]byte, NonceSize)
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return id, nil, err
		}
	} else if len(nonce) != NonceSize {
		return id, nil, errors.New("bad nonce size")
	}
	aead, err := chacha20poly1305.NewX(k.Enc[:])
	if err != nil {
		return id, nil, err
	}
	out := make([]byte, 0, blobHeader+NonceSize+len(body)+chacha20poly1305.Overhead+trailerSize)
	out = append(out, blobMagic...)
	out = append(out, blobVersion, flags, 0, 0)
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, body, blobAAD(flags, id))
	sum := sha256.Sum256(out)
	out = append(out, sum[:]...)
	return id, out, nil
}

// VerifyBlob checks structure and the SHA-256 trailer. It needs no key, so
// servers and the sync engine can use it.
func VerifyBlob(blob []byte) error {
	if len(blob) < blobOverhead {
		return errors.New("blob too short")
	}
	if string(blob[:4]) != blobMagic {
		return errors.New("bad blob magic")
	}
	if blob[4] != blobVersion {
		return fmt.Errorf("unsupported blob version %d", blob[4])
	}
	body, tr := blob[:len(blob)-trailerSize], blob[len(blob)-trailerSize:]
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare(sum[:], tr) != 1 {
		return errors.New("blob trailer digest mismatch")
	}
	return nil
}

// DecodeChunk verifies, decrypts and decompresses blob. want, if non-zero, is
// the id the blob is expected to carry; the keyed id of the decoded plaintext is
// always recomputed and compared.
func (k *Keys) DecodeChunk(blob []byte, want ID) ([]byte, error) {
	if err := VerifyBlob(blob); err != nil {
		return nil, err
	}
	flags := blob[5]
	if flags&^byte(flagZstd) != 0 || blob[6] != 0 || blob[7] != 0 {
		return nil, errors.New("unsupported blob flags")
	}
	nonce := blob[blobHeader : blobHeader+NonceSize]
	ct := blob[blobHeader+NonceSize : len(blob)-trailerSize]
	aead, err := chacha20poly1305.NewX(k.Enc[:])
	if err != nil {
		return nil, err
	}
	// The id is part of the AAD; it must be supplied by the caller (it is the
	// object name on the server). When unknown, decoding is impossible.
	if want.IsZero() {
		return nil, errors.New("chunk id required to decode")
	}
	var dst []byte
	if flags&flagZstd != 0 {
		// the decrypted compressed form is scratch: only the decompressed plaintext is returned
		bp := scratch.Get().(*[]byte)
		defer func() { *bp = dst[:0]; scratch.Put(bp) }()
		dst = (*bp)[:0]
	}
	pt, err := aead.Open(dst, nonce, ct, blobAAD(flags, want))
	if err != nil {
		return nil, errors.New("chunk authentication failed (wrong key, wrong id or corruption)")
	}
	if flags&flagZstd != 0 {
		dst = pt
		d := getZDec()
		pt, err = d.DecodeAll(pt, nil)
		putZDec(d)
		if err != nil {
			return nil, fmt.Errorf("decompressing chunk: %w", err)
		}
	}
	if got := k.ChunkID(pt); got != want {
		return nil, errors.New("chunk id does not match its content")
	}
	return pt, nil
}

// ---- helpers shared with index/manifest ------------------------------------

func putU64(b []byte, v uint64) []byte { return binary.BigEndian.AppendUint64(b, v) }
func putU32(b []byte, v uint32) []byte { return binary.BigEndian.AppendUint32(b, v) }

func appendTrailer(b []byte) []byte {
	sum := sha256.Sum256(b)
	return append(b, sum[:]...)
}

func checkTrailer(b []byte) ([]byte, error) {
	if len(b) < trailerSize {
		return nil, errors.New("too short")
	}
	body, tr := b[:len(b)-trailerSize], b[len(b)-trailerSize:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], tr) {
		return nil, errors.New("trailer digest mismatch")
	}
	return body, nil
}
