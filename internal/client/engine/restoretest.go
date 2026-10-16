package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/fscat"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/nbd"
)

// PublicError is an error whose message is safe to send to a server: it never contains a name from
// inside a volume (file names, paths). The wrapped error keeps the full detail for local logs.
type PublicError struct {
	Msg string
	Err error
}

func (e *PublicError) Error() string { return e.Msg }
func (e *PublicError) Unwrap() error { return e.Err }

// Detail returns the full text of err for LOCAL logs (it may name files inside volumes).
func Detail(err error) string {
	var pe *PublicError
	if errors.As(err, &pe) && pe.Err != nil {
		return pe.Msg + ": " + Detail(pe.Err)
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func public(err error, f string, a ...any) error {
	return &PublicError{Msg: fmt.Sprintf(f, a...), Err: err}
}

// RestoreTestOptions select what a restore test checks.
type RestoreTestOptions struct {
	Server   string // target name ("" = the first)
	Project  string
	Name     string // logical object name, e.g. "instance/vm1/root", "volume/biggi"
	Snapshot string // "" = latest
	Mode     string // jobspec.ModeDeep | ModeExtract | ModeFull
	// ScratchDir is where extract tests create their scratch directory (the client state dir).
	ScratchDir string
	// Pool and ScratchProject place the scratch Incus volume of a full test (Project when empty).
	Pool           string
	ScratchProject string
	// Progress, when set, is called with bytes done / total.
	Progress func(done, total int64)
}

// Scratch Incus objects of restore tests are named with this prefix (and never left behind).
const RestoreTestPrefix = "ibk-rt-"

// RestoreTest proves that one snapshot can really be restored, without touching the original object:
//
//   - deep: every chunk of the snapshot is downloaded, decrypted and its keyed id recomputed;
//   - extract (filesystem): the snapshot is extracted into a scratch directory below ScratchDir,
//     every file is re-read and checked against the SHA-256 in the catalog, and the entries on disk are
//     counted against the catalog; the scratch directory is always deleted;
//   - full: the snapshot is restored through the normal restore path into a scratch Incus volume;
//     for a block object the volume content is read back and hashed and compared with the hash of
//     the decrypted chunks, for a filesystem object the restored tree is checked like extract. The
//     scratch volume is ALWAYS deleted, on success, failure and cancellation.
//
// The result never contains a name from inside the volume. err is nil exactly when the test passed;
// otherwise it is a *PublicError (safe to report) whose wrapped error has the local detail.
func (e *Engine) RestoreTest(ctx context.Context, o RestoreTestOptions) (*jobspec.RestoreTestResult, error) {
	start := time.Now()
	res := &jobspec.RestoreTestResult{Project: o.Project, Object: o.Name, Snapshot: o.Snapshot, Method: o.Mode, Details: map[string]any{}}
	fail := func(err error) (*jobspec.RestoreTestResult, error) {
		res.Passed = false
		res.DurationMs = time.Since(start).Milliseconds()
		var pe *PublicError
		if !errors.As(err, &pe) {
			err = public(err, "restore test failed")
		}
		res.Details["error"] = err.Error()
		return res, err
	}
	switch o.Mode {
	case jobspec.ModeDeep, jobspec.ModeExtract, jobspec.ModeFull:
	default:
		return fail(public(nil, "unknown restore test mode %q", o.Mode))
	}
	t, err := e.PickTarget(o.Server)
	if err != nil {
		return fail(public(err, "%v", err))
	}
	loc, err := e.Locate(ctx, t, o.Project, o.Name, o.Snapshot)
	if err != nil {
		return fail(public(err, "the snapshot cannot be located or its manifest does not check: %v", err))
	}
	res.Snapshot = loc.Snapshot
	res.Details["kind"] = loc.Kind
	res.Details["logical_bytes"] = loc.Index.Size
	switch {
	case o.Mode == jobspec.ModeDeep:
		err = e.rtDeep(ctx, loc, o, res)
	case o.Mode == jobspec.ModeExtract:
		if loc.Kind != KindFilesystem {
			return fail(public(nil, "extract tests need a filesystem snapshot; %s is a %s object (use deep or full)", o.Name, loc.Kind))
		}
		err = e.rtExtract(ctx, loc, o, res)
	case loc.Kind == KindBlock:
		err = e.rtFullBlock(ctx, loc, o, res)
	case loc.Kind == KindFilesystem:
		err = e.rtFullFS(ctx, loc, o, res)
	default:
		return fail(public(nil, "full tests need a block or filesystem object; %s is a %s object (use deep)", o.Name, loc.Kind))
	}
	if err != nil {
		return fail(err)
	}
	res.Passed = true
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

const maxListedProblems = 50

// rtDeep fetches and decrypts every distinct chunk of the snapshot (in parallel) and records every
// bad chunk by id, not just the first.
func (e *Engine) rtDeep(ctx context.Context, loc *Located, o RestoreTestOptions, res *jobspec.RestoreTestResult) error {
	idx := loc.Index
	type job struct {
		id   format.ID
		size uint64 // fixed indexes: the length the plaintext must have (0 = any)
	}
	// the distinct chunks are fed to the workers while the index is walked: no job list is built
	// first pass: the distinct chunks and their total size; the second pass feeds each one once
	// (removing it from the set), so only the set exists, never a list of jobs
	seen := make(map[format.ID]struct{}, min(len(idx.Entries), 1<<16))
	var total int64
	for i, ent := range idx.Entries {
		if ent.ID.IsZero() {
			continue
		}
		if _, ok := seen[ent.ID]; ok {
			continue
		}
		seen[ent.ID] = struct{}{}
		_, n := idx.Span(i)
		total += int64(n)
	}
	nJobs := 0
	var mu sync.Mutex
	var done int64
	var problems []string
	bad := 0
	ch := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < e.workers(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				var msg string
				blob, err := loc.Target.Client.Chunk(ctx, j.id.String())
				if err != nil {
					msg = fmt.Sprintf("chunk %s: cannot be fetched: %v", j.id, err)
				} else if pt, derr := loc.Target.Keys.DecodeChunk(blob, j.id); derr != nil {
					msg = fmt.Sprintf("chunk %s: does not decrypt to its id: %v", j.id, derr)
				} else if j.size != 0 && uint64(len(pt)) != j.size {
					msg = fmt.Sprintf("chunk %s: has %d bytes, the index says %d", j.id, len(pt), j.size)
				} else {
					mu.Lock()
					done += int64(len(pt))
					d := done
					mu.Unlock()
					if o.Progress != nil {
						o.Progress(d, total)
					}
				}
				if msg != "" {
					mu.Lock()
					bad++
					if len(problems) < maxListedProblems {
						problems = append(problems, msg)
					}
					mu.Unlock()
				}
			}
		}()
	}
feed:
	for i, ent := range idx.Entries {
		if _, ok := seen[ent.ID]; !ok {
			continue // zero, or already fed
		}
		delete(seen, ent.ID)
		_, n := idx.Span(i)
		j := job{id: ent.ID}
		if idx.Kind == format.IndexFixed {
			j.size = n
		}
		nJobs++
		select {
		case ch <- j:
		case <-ctx.Done():
			break feed
		}
	}
	seen = nil
	close(ch)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return public(err, "restore test interrupted: %v", err)
	}
	sort.Strings(problems)
	res.Chunks = nJobs
	res.Bytes = done
	if bad > 0 {
		res.Details["bad_chunks"] = bad
		res.Details["problems"] = problems
		return public(nil, "%d of %d chunks failed (first: %s)", bad, nJobs, problems[0])
	}
	return nil
}

// rtExtract restores a filesystem snapshot into a scratch directory and checks it.
func (e *Engine) rtExtract(ctx context.Context, loc *Located, o RestoreTestOptions, res *jobspec.RestoreTestResult) error {
	if o.ScratchDir == "" {
		return public(nil, "no scratch directory configured for extract tests")
	}
	if err := os.MkdirAll(o.ScratchDir, 0o700); err != nil {
		return public(err, "cannot create the scratch area")
	}
	need := loc.Index.Size
	// 63 bits: the payload size is later used as an int64 (progress), so a larger number is not a size
	if p, err := strconv.ParseUint(loc.Manifest.Meta[MetaFSPayload], 10, 63); err == nil {
		need = p
	}
	var st unix.Statfs_t
	if err := unix.Statfs(o.ScratchDir, &st); err == nil {
		free := st.Bavail * uint64(st.Bsize)
		if free < need+need/20+64<<20 {
			return public(nil, "not enough scratch space for an extract test: %d bytes free, about %d needed", free, need)
		}
	}
	dir, err := os.MkdirTemp(o.ScratchDir, RestoreTestPrefix+"*")
	if err != nil {
		return public(err, "cannot create the scratch directory")
	}
	defer removeAll(dir)
	return e.rtRestoreAndCheck(ctx, loc, dir, o, res)
}

// rtRestoreAndCheck restores loc into dir (catalog or tar format) and checks what landed on disk.
func (e *Engine) rtRestoreAndCheck(ctx context.Context, loc *Located, dir string, o RestoreTestOptions, res *jobspec.RestoreTestResult) error {
	noChown := os.Geteuid() != 0
	if !IsCatalogSnapshot(loc) {
		// archive format: no per-file checksums exist; the chunks decrypt to their keyed ids (AEAD)
		var done int64
		total := int64(loc.Index.Size)
		pr, pw := io.Pipe()
		errc := make(chan error, 1)
		go func() { errc <- extractTar(pr, dir, noChown); _, _ = io.Copy(io.Discard, pr) }()
		serr := e.stream(ctx, loc.Target, loc.Index, func(_ int, data []byte) error {
			done += int64(len(data))
			if o.Progress != nil {
				o.Progress(done, total)
			}
			_, werr := pw.Write(data)
			return werr
		})
		_ = pw.CloseWithError(serr)
		xerr := <-errc
		if serr != nil {
			return public(serr, "restoring the snapshot failed: %v", serr)
		}
		if xerr != nil {
			return public(xerr, "the archive could not be extracted")
		}
		files, _, _ := countTree(dir)
		res.Files, res.Bytes, res.Chunks = files, done, len(loc.Index.ChunkIDs())
		res.Details["format"] = "tar"
		res.Details["note"] = "archive-format snapshot: no per-file checksums exist; content verified through the chunk ids (AEAD) only"
		return nil
	}
	tree, pay, err := e.loadCatalog(ctx, loc)
	if err != nil {
		var pe *PublicError
		if errors.As(err, &pe) {
			return err
		}
		return public(err, "the catalog cannot be read: %v", err) // chunk ids and digests only
	}
	pi, err := subIndex(loc.Index, 0, pay)
	if err != nil && pay != 0 {
		return public(err, "the payload range of the index is inconsistent")
	}
	var done int64
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		errc <- tree.Extract(dir, fscat.Sequential(pr), fscat.ExtractOptions{NoChown: noChown})
		_, _ = io.Copy(io.Discard, pr)
	}()
	var serr error
	if pi != nil && len(pi.Entries) > 0 {
		serr = e.stream(ctx, loc.Target, pi, func(_ int, data []byte) error {
			done += int64(len(data))
			if o.Progress != nil {
				o.Progress(done, int64(pay))
			}
			_, werr := pw.Write(data)
			return werr
		})
	}
	_ = pw.CloseWithError(serr)
	xerr := <-errc
	res.Chunks = len(loc.Index.ChunkIDs())
	res.Bytes = done
	if serr != nil {
		// stream errors name chunk ids, never files
		return public(serr, "restoring the snapshot failed: %v", serr)
	}
	if xerr != nil {
		return public(xerr, "extraction failed (a file did not match the catalog or could not be written; see the host log)")
	}
	return checkAgainstCatalog(tree, dir, noChown, res)
}

// checkAgainstCatalog re-reads every regular file below dir and compares it (and the entry counts)
// with the catalog. Problems are counted by kind; names stay in the local detail only.
func checkAgainstCatalog(tree *fscat.Tree, dir string, noChown bool, res *jobspec.RestoreTestResult) error {
	want := tree.Stats()
	var mismatched, missing int
	var detail []string
	tree.Walk(func(rel string, en *fscat.Entry) bool {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		switch en.Kind {
		case fscat.KFile:
			ok, err := fileMatches(p, en)
			if err != nil {
				missing++
				detail = append(detail, rel+": "+err.Error())
			} else if !ok {
				mismatched++
				detail = append(detail, rel+": content differs from the catalog")
			}
		case fscat.KSymlink:
			if l, err := os.Readlink(p); err != nil || l != en.Link {
				mismatched++
				detail = append(detail, rel+": symlink differs")
			}
		case fscat.KDir:
			if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
				missing++
				detail = append(detail, rel+": directory missing")
			}
		case fscat.KHardlink:
			if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
				missing++
				detail = append(detail, rel+": hard link missing")
			}
		}
		return true
	})
	files, dirs, links := countTree(dir)
	res.Files = files
	res.Details["catalog_files"] = want.Files + want.Hardlinks
	res.Details["catalog_dirs"] = want.Dirs
	res.Details["catalog_symlinks"] = want.Symlinks
	res.Details["catalog_special"] = want.Special
	res.Details["disk_files"] = files
	res.Details["disk_dirs"] = dirs
	res.Details["disk_symlinks"] = links
	res.Details["content_bytes"] = want.Bytes
	res.Details["mismatched"] = mismatched
	res.Details["missing"] = missing
	countsOK := files == int64(want.Files+want.Hardlinks) && dirs == int64(want.Dirs) && links == int64(want.Symlinks)
	if mismatched > 0 || missing > 0 || !countsOK {
		return public(errors.New(joinMax(detail, 20)), "the restored tree does not match the catalog: %d mismatched, %d missing; on disk %d files/%d dirs/%d symlinks, catalog %d/%d/%d",
			mismatched, missing, files, dirs, links, want.Files+want.Hardlinks, want.Dirs, want.Symlinks)
	}
	return nil
}

func joinMax(s []string, n int) string {
	if len(s) > n {
		s = append(s[:n:n], fmt.Sprintf("... and %d more", len(s)-n))
	}
	out := ""
	for i, x := range s {
		if i > 0 {
			out += "; "
		}
		out += x
	}
	return out
}

func fileMatches(p string, en *fscat.Entry) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, errors.New("not a regular file")
	}
	if uint64(fi.Size()) != en.Size {
		return false, nil
	}
	if en.Size == 0 {
		return true, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	return got == en.Sum, nil
}

// countTree counts regular files, directories (including dir itself) and symlinks below dir.
func countTree(dir string) (files, dirs, links int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		switch {
		case d.Type().IsRegular():
			files++
		case d.IsDir():
			dirs++
		case d.Type()&fs.ModeSymlink != 0:
			links++
		}
		return nil
	})
	return
}

// removeAll deletes a scratch tree even when restored directories are read-only for this user.
func removeAll(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}

func scratchName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return RestoreTestPrefix + hex.EncodeToString(b[:])
}

// withScratchVolume creates a scratch custom volume, runs fn on it and deletes it on EVERY path
// (with a fresh context: the job's may already be cancelled).
func (e *Engine) withScratchVolume(ctx context.Context, spec incusx.VolumeSpec, fn func(ref incusx.VolRef) error) error {
	ref := incusx.VolRef{Pool: spec.Pool, Project: spec.Project, Type: "custom", Name: spec.Name}
	created := true // set BEFORE the call: a call that fails half-way may still have created the volume
	defer func() {
		if !created {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := e.Inc.DeleteVolume(cctx, ref); err != nil {
			e.Log.Warn("could not delete a restore-test scratch volume; remove it by hand", "volume", ref.Name, "project", ref.Project, "pool", ref.Pool, "err", err)
		}
	}()
	if err := e.Inc.CreateVolume(ctx, spec); err != nil {
		if ok, xerr := e.Inc.VolumeExists(context.Background(), ref); xerr == nil && !ok {
			created = false
		}
		return public(err, "creating the scratch volume failed: %v", err)
	}
	return fn(ref)
}

func (e *Engine) scratchSpec(o RestoreTestOptions, loc *Located, content string) (incusx.VolumeSpec, error) {
	if o.Pool == "" {
		return incusx.VolumeSpec{}, public(nil, "a storage pool for the scratch volume is required (recovery.pool in the configuration)")
	}
	project := o.ScratchProject
	if project == "" {
		project = o.Project
	}
	return incusx.VolumeSpec{Pool: o.Pool, Project: project, Name: scratchName(), ContentType: content, Size: int64(loc.Manifest.Object.Size)}, nil
}

// rtFullBlock restores a block snapshot into a scratch volume through the normal NBD write path,
// computing the hash of the decrypted content on the way, then reads the volume back over a new
// connection and compares.
func (e *Engine) rtFullBlock(ctx context.Context, loc *Located, o RestoreTestOptions, res *jobspec.RestoreTestResult) error {
	spec, err := e.scratchSpec(o, loc, "block")
	if err != nil {
		return err
	}
	if spec.Size < int64(loc.Index.Size) {
		spec.Size = int64(loc.Index.Size)
	}
	size := loc.Index.Size
	total := int64(2 * size)
	return e.withScratchVolume(ctx, spec, func(ref incusx.VolRef) error {
		want := sha256.New()
		if err := func() error {
			nc, err := e.Inc.VolumeNBD(ctx, ref, true)
			if err != nil {
				return public(err, "opening the scratch volume for writing failed: %v", err)
			}
			c, err := nbd.Dial(ctx, nc, "", nil)
			if err != nil {
				return public(err, "NBD to the scratch volume failed: %v", err)
			}
			defer c.Close()
			if c.Size() < size {
				return public(nil, "the scratch volume is %d bytes, the backup is %d", c.Size(), size)
			}
			// the production restore path (RestoreBlock, as writeVolume uses it), through a writer that
			// also hashes what is written; zero chunks it skips on the fresh volume are hashed as zeros
			hw := &hashingWriterAt{w: c, h: want}
			err = e.RestoreBlock(ctx, loc, hw, true, func(done, _ uint64) {
				if o.Progress != nil {
					o.Progress(int64(done), total)
				}
			})
			if err == nil {
				err = hw.padTo(size)
			}
			if err != nil {
				return public(err, "restoring into the scratch volume failed: %v", err)
			}
			if err := c.Flush(); err != nil {
				return public(err, "flushing the scratch volume failed: %v", err)
			}
			return nil
		}(); err != nil {
			return err
		}
		nc, err := e.Inc.VolumeNBD(ctx, ref, false)
		if err != nil {
			return public(err, "opening the scratch volume for reading failed: %v", err)
		}
		c, err := nbd.Dial(ctx, nc, "", nil)
		if err != nil {
			return public(err, "NBD to the scratch volume failed: %v", err)
		}
		defer c.Close()
		got := sha256.New()
		buf := make([]byte, 4<<20)
		for off := uint64(0); off < size; {
			if err := ctx.Err(); err != nil {
				return public(err, "restore test interrupted: %v", err)
			}
			n := uint64(len(buf))
			if size-off < n {
				n = size - off
			}
			if _, err := c.ReadAt(buf[:n], int64(off)); err != nil {
				return public(err, "reading the scratch volume back failed: %v", err)
			}
			got.Write(buf[:n])
			off += n
			if o.Progress != nil {
				o.Progress(int64(size+off), total)
			}
		}
		w, g := hex.EncodeToString(want.Sum(nil)), hex.EncodeToString(got.Sum(nil))
		res.Bytes = int64(size)
		res.Chunks = len(loc.Index.ChunkIDs())
		res.Details["expected_sha256"] = w
		res.Details["volume_sha256"] = g
		if w != g {
			return public(nil, "the restored volume differs from the backup (sha256 %s, expected %s)", g, w)
		}
		return nil
	})
}

// hashingWriterAt forwards in-order writes and hashes the byte stream they form, hashing zeros for
// the gaps (zero chunks RestoreBlock does not write to a fresh volume).
type hashingWriterAt struct {
	w    io.WriterAt
	h    io.Writer
	next uint64
}

func (x *hashingWriterAt) zeros(n uint64) {
	for n > 0 {
		k := uint64(len(zeroBuf))
		if n < k {
			k = n
		}
		x.h.Write(zeroBuf[:k])
		n -= k
	}
}

func (x *hashingWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || uint64(off) < x.next {
		return 0, fmt.Errorf("restore wrote out of order at %d (expected >= %d)", off, x.next)
	}
	x.zeros(uint64(off) - x.next)
	n, err := x.w.WriteAt(p, off)
	x.h.Write(p[:n])
	x.next = uint64(off) + uint64(n)
	return n, err
}

func (x *hashingWriterAt) padTo(size uint64) error {
	if x.next > size {
		return fmt.Errorf("restore wrote %d bytes past the end (%d)", x.next, size)
	}
	x.zeros(size - x.next)
	x.next = size
	return nil
}

// rtFullFS restores a filesystem snapshot into a scratch filesystem volume and checks the tree.
func (e *Engine) rtFullFS(ctx context.Context, loc *Located, o RestoreTestOptions, res *jobspec.RestoreTestResult) error {
	spec, err := e.scratchSpec(o, loc, "filesystem")
	if err != nil {
		return err
	}
	return e.withScratchVolume(ctx, spec, func(ref incusx.VolRef) error {
		dir, release, err := e.Inc.FilesystemPath(ctx, ref)
		if err != nil {
			return public(err, "the scratch volume cannot be mounted: %v", err)
		}
		defer release()
		return e.rtRestoreAndCheck(ctx, loc, dir, o, res)
	})
}
