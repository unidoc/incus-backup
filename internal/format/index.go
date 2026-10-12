package format

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// Index file layout (big endian), format version 1. An index is plaintext: it
// lists keyed chunk ids and offsets only, so a keyless server can run dedup,
// GC and sync from it.
//
//	magic       4  "IBIX"
//	version     1  0x01
//	kind        1  1 = fixed size chunks, 2 = dynamic (content defined)
//	reserved    2  0
//	chunk_size  4  fixed: chunk size in bytes; dynamic: 0
//	total_size  8  logical size of the object in bytes
//	count       8  number of entries
//	entries     count * (end u64 | id 32 bytes)
//	trailer    32  SHA-256 over every preceding byte
//
// Entry i covers [end(i-1), end(i)). A zero id means "all zero bytes, no blob".
const (
	indexMagic   = "IBIX"
	indexVersion = 1
	indexHeader  = 4 + 1 + 1 + 2 + 4 + 8 + 8
	entrySize    = 8 + IDSize

	// IndexFixed and IndexDynamic are the index kinds.
	IndexFixed   = 1
	IndexDynamic = 2

	// FixedChunkSize is the chunk size for block objects.
	FixedChunkSize = 4 << 20
)

// Entry is one chunk reference.
type Entry struct {
	End uint64
	ID  ID
}

// Index is a parsed index file.
type Index struct {
	Kind      int
	ChunkSize uint32
	Size      uint64
	Entries   []Entry
}

// Validate checks internal consistency.
func (x *Index) Validate() error {
	if x.Kind != IndexFixed && x.Kind != IndexDynamic {
		return fmt.Errorf("unknown index kind %d", x.Kind)
	}
	var prev uint64
	for i, e := range x.Entries {
		if e.End <= prev {
			return fmt.Errorf("entry %d: end %d not after %d", i, e.End, prev)
		}
		if x.Kind == IndexFixed {
			want := (uint64(i) + 1) * uint64(x.ChunkSize)
			if want > x.Size {
				want = x.Size
			}
			if e.End != want {
				return fmt.Errorf("entry %d: end %d, want %d for fixed chunking", i, e.End, want)
			}
		}
		prev = e.End
	}
	if len(x.Entries) == 0 {
		if x.Size != 0 {
			return errors.New("no entries but non-zero size")
		}
		return nil
	}
	if prev != x.Size {
		return fmt.Errorf("last entry ends at %d, size is %d", prev, x.Size)
	}
	if x.Kind == IndexFixed && x.ChunkSize == 0 {
		return errors.New("fixed index needs a chunk size")
	}
	return nil
}

// Marshal serialises the index.
func (x *Index) Marshal() ([]byte, error) {
	if err := x.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, 0, indexHeader+len(x.Entries)*entrySize+trailerSize)
	b = append(b, indexMagic...)
	b = append(b, indexVersion, byte(x.Kind), 0, 0)
	b = putU32(b, x.ChunkSize)
	b = putU64(b, x.Size)
	b = putU64(b, uint64(len(x.Entries)))
	for _, e := range x.Entries {
		b = putU64(b, e.End)
		b = append(b, e.ID[:]...)
	}
	return appendTrailer(b), nil
}

// ParseIndex parses and validates an index file.
func ParseIndex(b []byte) (*Index, error) {
	body, err := checkTrailer(b)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if len(body) < indexHeader || string(body[:4]) != indexMagic {
		return nil, errors.New("index: bad magic")
	}
	if body[4] != indexVersion {
		return nil, fmt.Errorf("index: unsupported version %d", body[4])
	}
	if body[6] != 0 || body[7] != 0 {
		return nil, errors.New("index: reserved bytes set")
	}
	x := &Index{Kind: int(body[5])}
	x.ChunkSize = binary.BigEndian.Uint32(body[8:])
	x.Size = binary.BigEndian.Uint64(body[12:])
	count := binary.BigEndian.Uint64(body[20:])
	rest := body[indexHeader:]
	if count > uint64(len(rest))/entrySize || uint64(len(rest)) != count*entrySize {
		return nil, errors.New("index: entry count does not match length")
	}
	x.Entries = make([]Entry, count)
	for i := range x.Entries {
		o := i * entrySize
		x.Entries[i].End = binary.BigEndian.Uint64(rest[o:])
		copy(x.Entries[i].ID[:], rest[o+8:o+entrySize])
	}
	if err := x.Validate(); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return x, nil
}

// Digest returns the hex SHA-256 of the serialised index file (what manifests
// record).
func IndexDigest(file []byte) string {
	s := sha256.Sum256(file)
	return hex.EncodeToString(s[:])
}

// ChunkIDs returns the distinct non-zero ids referenced by the index, in index order.
//
// The set of ids seen keys on the first 8 bytes of an id and points at its place in the result
// (about 12 bytes per distinct id instead of a 32-byte key with map overhead): a prefix match is
// confirmed against the full id, and the rare different id with the same prefix goes to an exact
// overflow set, so the result is exact.
func (x *Index) ChunkIDs() []ID {
	pos := make(map[uint64]int32, min(len(x.Entries), 1<<16))
	var overflow map[ID]struct{}
	var out []ID
	for _, e := range x.Entries {
		if e.ID.IsZero() {
			continue
		}
		k := binary.LittleEndian.Uint64(e.ID[:8])
		if j, ok := pos[k]; ok {
			if out[j] == e.ID {
				continue
			}
			if overflow == nil {
				overflow = map[ID]struct{}{}
			}
			if _, ok := overflow[e.ID]; ok {
				continue
			}
			overflow[e.ID] = struct{}{}
			out = append(out, e.ID)
			continue
		}
		pos[k] = int32(len(out))
		out = append(out, e.ID)
	}
	return out
}

// NewFixedIndex builds an empty fixed-size index skeleton for an object of
// the given size where every entry is the zero chunk.
func NewFixedIndex(size uint64, chunkSize uint32) *Index {
	x := &Index{Kind: IndexFixed, ChunkSize: chunkSize, Size: size}
	for off := uint64(0); off < size; off += uint64(chunkSize) {
		end := off + uint64(chunkSize)
		if end > size {
			end = size
		}
		x.Entries = append(x.Entries, Entry{End: end})
	}
	return x
}

// Span returns the byte range covered by fixed entry i.
func (x *Index) Span(i int) (off, n uint64) {
	if i > 0 {
		off = x.Entries[i-1].End
	}
	return off, x.Entries[i].End - off
}
