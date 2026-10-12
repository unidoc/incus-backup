// Package webfixture_test builds the test repository of the browser library (web-crypto/) with
// the REAL backup engine: a fake Incus (internal/client/incusx/fake) serves directories and a VM
// disk, the engine backs them up over mutual TLS into a real in-process server
// (internal/testsupport: PostgreSQL + filesystem datastore), and the committed snapshots are read
// back through the real client API and written to disk under the paths the browser's HttpFetcher
// uses (api/v1/snapshots/{group}/{snap}/index|manifest, api/v1/chunks/{id}).
//
// With IB_WEBFIXTURE_OUT set the repository is written there (web-crypto's test setup does
// this); otherwise into a temporary directory, so the generator itself is checked by go test.
package webfixture_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const MiB = 1 << 20

func rnd(n int, seed int64) []byte {
	b := make([]byte, n)
	mrand.New(mrand.NewSource(seed)).Read(b)
	return b
}

func write(t *testing.T, root, rel string, data []byte, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// buildDocs creates the catalog-snapshot source tree. It returns notes about optional features.
func buildDocs(t *testing.T, root string) map[string]any {
	t.Helper()
	notes := map[string]any{}
	write(t, root, "big/random-30MiB.bin", rnd(30*MiB, 1), 0o644)
	write(t, root, "big/zeros-3MiB.bin", make([]byte, 3*MiB), 0o644)
	mixed := append(append(rnd(1*MiB, 2), make([]byte, 9*MiB)...), rnd(1*MiB+123, 3)...)
	write(t, root, "big/mixed-zero-hole.bin", mixed, 0o644)
	write(t, root, "big/medium-700KiB.bin", rnd(700<<10, 4), 0o644)
	r := mrand.New(mrand.NewSource(5))
	for i := 0; i < 400; i++ {
		n := r.Intn(20 << 10)
		if i%37 == 0 {
			n = 0
		}
		write(t, root, fmt.Sprintf("small/f%03d.txt", i), []byte(strings.Repeat(fmt.Sprintf("small file %d;", i), n/10+1)[:n]), 0o644)
	}
	write(t, root, "Ünïcødé/日本語 ファイル.txt", []byte("こんにちは世界\n"), 0o644)
	write(t, root, "Ünïcødé/emoji 🎉 party.txt", []byte("🎉🎉🎉\n"), 0o644)
	write(t, root, "Ünïcødé/ß-straße/ä ö ü.txt", []byte("Grüße\n"), 0o644)
	write(t, root, "space name.txt", []byte("spaces\n"), 0o644)
	write(t, root, "deep/a/b/c/d/e/f/leaf.txt", []byte("leaf\n"), 0o644)
	write(t, root, "empty.txt", nil, 0o644)
	write(t, root, "deep/a/empty-inside.txt", nil, 0o600)
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, "perms/secret.txt", []byte("top secret\n"), 0o600)
	write(t, root, "perms/script.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	// symlinks: relative, absolute, dangling, to a directory
	if err := os.MkdirAll(filepath.Join(root, "links"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"rel": "../small/f001.txt", "abs": "/etc/hostname", "dangling": "does/not/exist", "dirlink": "../deep"} {
		if err := os.Symlink(target, filepath.Join(root, "links", name)); err != nil {
			t.Fatal(err)
		}
	}
	// hard links: the first in walk order holds the content, the others name it
	write(t, root, "hard/original.txt", []byte("one inode, three names\n"), 0o644)
	for _, l := range []string{"hard/second-link.txt", "other/third-link.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, l)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(root, "hard/original.txt"), filepath.Join(root, l)); err != nil {
			t.Fatal(err)
		}
	}
	// a special file: browsable, not extractable into a ZIP
	if err := os.MkdirAll(filepath.Join(root, "special"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "special/pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	// an extended attribute where the filesystem allows it
	xp := filepath.Join(root, "perms/secret.txt")
	if err := unix.Setxattr(xp, "user.comment", []byte("xattr value"), 0); err == nil {
		notes["xattr"] = map[string]string{"path": "perms/secret.txt", "name": "user.comment", "value": "xattr value"}
	} else {
		notes["xattr"] = nil
		notes["xattrError"] = err.Error()
	}
	// fixed mtimes on a few entries so tests can check them exactly
	mt := time.Date(2021, 3, 4, 5, 6, 7, 123456789, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "space name.txt"), mt, mt); err != nil {
		t.Fatal(err)
	}
	notes["mtime"] = map[string]string{"path": "space name.txt", "ns": fmt.Sprint(mt.UnixNano())}
	return notes
}

// buildHostile creates a tree whose names and modes are what a hostile guest tenant can create on
// Linux, to test that browser ZIPs neutralise them (web-crypto/src/zip.ts): backslashes, control
// and escape characters, a bidi override, a drive-letter-like directory, case collisions, a
// symlink that collides with a directory of the same name in another case, setuid/setgid/sticky.
func buildHostile(t *testing.T, root string) {
	t.Helper()
	write(t, root, `..\..\AppData\evil.bat`, []byte("@echo off\n"), 0o644)
	write(t, root, "ctrl\x01\x1b[31mred.txt", []byte("control\n"), 0o644)
	write(t, root, "invoice\u202etxt.exe", []byte("bidi\n"), 0o644)
	write(t, root, "C:/win.ini", []byte("[drive]\n"), 0o644)
	write(t, root, "Readme.txt", []byte("one\n"), 0o644)
	write(t, root, "README.TXT", []byte("two\n"), 0o644)
	write(t, root, "SSH/authorized_keys", []byte("ssh-ed25519 AAAA attacker\n"), 0o600)
	if err := os.Symlink("/root/.ssh", filepath.Join(root, "ssh")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "long/"+strings.Repeat("n", 250)+".txt", []byte("long\n"), 0o644)
	write(t, root, "modes/suid.sh", []byte("#!/bin/sh\n"), 0o755)
	for p, m := range map[string]os.FileMode{"modes/suid.sh": 0o755 | os.ModeSetuid, "modes": 0o775 | os.ModeSetgid, "tmp": 0o777 | os.ModeSticky} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if p == "tmp" {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(full, m); err != nil {
			t.Fatal(err)
		}
	}
}

type snapOut struct {
	Label   string `json:"label"`
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Object  string `json:"object"`
	Group   string `json:"group"`
	Snap    string `json:"snap"`
	Format  string `json:"format"`
	Source  string `json:"source,omitempty"` // source tree (relative to the fixture root)
}

type sealedOut struct {
	File  string `json:"file"`
	Plain string `json:"plain"`
	Size  int    `json:"size"`
}

type fixture struct {
	RecoveryKey      string               `json:"recoveryKey"`
	WrongRecoveryKey string               `json:"wrongRecoveryKey"`
	MasterHex        string               `json:"masterHex"` // tests only: the leak test searches for it
	DomainID         string               `json:"domainId"`
	Snapshots        []snapOut            `json:"snapshots"`
	Chunks           int                  `json:"chunks"`
	SealPrivateHex   string               `json:"sealPrivateHex"`
	SealPublicHex    string               `json:"sealPublicHex"`
	SealJobID        string               `json:"sealJobId"`  // the job every sealed fixture answers
	SealType         string               `json:"sealType"`   // its type and the parameters the "browser" sent:
	SealParams       string               `json:"sealParams"` // jobspec.RequestDigest(SealType, SealParams) is bound
	Sealed           map[string]sealedOut `json:"sealed"`
	Notes            map[string]any       `json:"notes"`
	EscrowFile       string               `json:"escrowFile"`
	EscrowPrivate    string               `json:"escrowPrivate"`
}

func TestGenerateWebFixture(t *testing.T) {
	out := os.Getenv("IB_WEBFIXTURE_OUT")
	if out == "" {
		out = t.TempDir()
	} else {
		// only ever replace an earlier fixture: a stray variable must not wipe an arbitrary directory
		if ents, err := os.ReadDir(out); err == nil && len(ents) > 0 {
			if _, err := os.Stat(filepath.Join(out, "fixture.json")); err != nil {
				t.Fatalf("IB_WEBFIXTURE_OUT=%s is not empty and holds no fixture.json: refusing to replace it", out)
			}
		}
		if err := os.RemoveAll(out); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	master, err := format.GenerateMaster()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := format.DeriveKeys(master)
	if err != nil {
		t.Fatal(err)
	}
	rk, _ := format.EncodeRecoveryKey(master)
	wrong, _ := format.GenerateMaster()
	wrk, _ := format.EncodeRecoveryKey(wrong)
	fx := &fixture{RecoveryKey: rk, WrongRecoveryKey: wrk, MasterHex: hex.EncodeToString(master), DomainID: keys.DomainID, Sealed: map[string]sealedOut{}}

	docs := filepath.Join(out, "source/docs")
	fx.Notes = buildDocs(t, docs)
	hostile := filepath.Join(out, "source/hostile")
	buildHostile(t, hostile)
	tarSrc := filepath.Join(out, "source/docs-tar")
	write(t, tarSrc, "readme.txt", []byte("archive format snapshot\n"), 0o644)
	write(t, tarSrc, "data.bin", rnd(300<<10, 9), 0o644)

	node := testsupport.NewNode(t)
	cr := node.Enrol("benfica", api.ScopeBackup)
	client := node.Client(cr, keys.DomainID)
	inc := fake.New(t.TempDir())
	inc.AddFSVolume("default", "docs", docs)
	inc.Vols["default/docs"].Config[engine.KeyPolicy] = "prod"
	inc.AddFSVolume("default", "hostile", hostile)
	inc.Vols["default/hostile"].Config[engine.KeyPolicy] = "prod"
	inc.AddFSVolume("default", "docs-tar", tarSrc)
	inc.Vols["default/docs-tar"].Config[engine.KeyPolicy] = "prod"
	inc.AddVM("default", "bosch", map[string]int{"root": 12 * MiB}, true)
	inc.SetConfig("default", "bosch", engine.KeyPolicy, "prod")
	inc.GuestWrite("default", "bosch", "root", 0, rnd(12*MiB, 10))

	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	e := &engine.Engine{Inc: inc, Targets: map[string]*engine.Target{"lab": {Name: "lab", Client: client, Keys: keys}}, Host: "benfica",
		SpoolDir: t.TempDir(), MaxFreeze: time.Minute, MaxLag: 7 * 24 * time.Hour, Workers: 3,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Policies: map[string]api.Policy{"prod": {Name: "prod", Targets: []string{"lab"}, CommitRequired: "all"}}}
	e.Now = func() time.Time { return now }
	objs, err := e.Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.Resolve(objs)
	byName := map[string]*engine.Object{}
	for _, o := range objs {
		byName[o.Name] = o
	}
	spool := func() *engine.Spool {
		sp, err := engine.NewSpool(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sp.Close)
		return sp
	}
	add := func(label string, o *engine.Object, snap, fmtName, src string) {
		fx.Snapshots = append(fx.Snapshots, snapOut{Label: label, Project: o.Project, Kind: o.Kind, Object: o.Name,
			Group: keys.GroupID(o.Project, o.Kind, o.Name), Snap: snap, Format: fmtName, Source: src})
	}

	// catalog snapshot (the default format)
	docsObj := byName["volume/docs"]
	if docsObj == nil {
		t.Fatalf("volume/docs not discovered: %+v", objs)
	}
	res := e.BackupFS(ctx, docsObj, spool())
	if !res.OK() {
		t.Fatalf("catalog backup: %v %+v", res.Err, res.Targets)
	}
	add("catalog", docsObj, res.Snapshot, "catalog", "source/docs")

	// a catalog snapshot of hostile names and modes
	now = now.Add(time.Minute)
	hostObj := byName["volume/hostile"]
	if hostObj == nil {
		t.Fatalf("volume/hostile not discovered")
	}
	res = e.BackupFS(ctx, hostObj, spool())
	if !res.OK() {
		t.Fatalf("hostile backup: %v %+v", res.Err, res.Targets)
	}
	add("hostile", hostObj, res.Snapshot, "catalog", "source/hostile")

	// archive-format snapshot
	now = now.Add(time.Minute)
	e.FSFormat = "tar"
	tarObj := byName["volume/docs-tar"]
	res = e.BackupFS(ctx, tarObj, spool())
	if !res.OK() {
		t.Fatalf("tar backup: %v %+v", res.Err, res.Targets)
	}
	add("archive", tarObj, res.Snapshot, "archive", "source/docs-tar")
	e.FSFormat = ""

	// block snapshot of a VM disk
	now = now.Add(time.Minute)
	root := byName["instance/bosch/root"]
	insts, _ := inc.Instances(ctx, "default")
	var inst incusx.Instance
	for _, i := range insts {
		if i.Name == "bosch" {
			inst = i
		}
	}
	snap, rs := e.BackupVM(ctx, inst, []engine.Disk{{Obj: root, Device: root.Device}}, spool())
	for _, r := range rs {
		if !r.OK() {
			t.Fatalf("block backup: %v %+v", r.Err, r.Targets)
		}
	}
	add("block", root, snap, "block", "")
	if err := os.WriteFile(filepath.Join(out, "block-root.img"), inc.Disk("default", "bosch", "root"), 0o644); err != nil {
		t.Fatal(err)
	}

	// export through the real client API under the browser's paths
	seen := map[string]bool{}
	for _, s := range fx.Snapshots {
		dir := filepath.Join(out, "api/v1/snapshots", s.Group, s.Snap)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		ib, err := client.Index(ctx, s.Group, s.Snap)
		if err != nil {
			t.Fatal(err)
		}
		mb, err := client.Manifest(ctx, s.Group, s.Snap)
		if err != nil {
			t.Fatal(err)
		}
		man, err := keys.OpenManifest(mb, s.Group, s.Snap)
		if err != nil {
			t.Fatal(err)
		}
		if man.IndexDigest != format.IndexDigest(ib) {
			t.Fatal("index digest")
		}
		gotFmt := map[bool]string{true: "catalog", false: "archive"}[man.Meta[engine.MetaFSFormat] == "2"]
		if man.Object.Kind == "block" {
			gotFmt = "block"
		}
		if gotFmt != s.Format {
			t.Fatalf("%s: manifest says %s", s.Label, gotFmt)
		}
		if err := os.WriteFile(filepath.Join(dir, "index"), ib, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest"), mb, 0o644); err != nil {
			t.Fatal(err)
		}
		idx, err := format.ParseIndex(ib)
		if err != nil {
			t.Fatal(err)
		}
		cdir := filepath.Join(out, "api/v1/chunks")
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, id := range idx.ChunkIDs() {
			if seen[id.String()] {
				continue
			}
			seen[id.String()] = true
			blob, err := client.Chunk(ctx, id.String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := keys.DecodeChunk(blob, id); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cdir, id.String()), blob, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	fx.Chunks = len(seen)

	// the catalog snapshot opens with the engine's own reader too
	loc, err := e.Locate(ctx, e.Targets["lab"], "default", "volume/docs", fx.Snapshots[0].Snap)
	if err != nil {
		t.Fatal(err)
	}
	view, err := e.OpenFS(ctx, loc)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := view.Cat(&buf, "big/random-30MiB.bin"); err != nil || !bytes.Equal(buf.Bytes(), rnd(30*MiB, 1)) {
		t.Fatalf("engine cat: %v", err)
	}

	// sealed streams produced by format.NewSealWriter
	sk, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fx.SealPrivateHex, fx.SealPublicHex = hex.EncodeToString(sk.Bytes()), hex.EncodeToString(sk.PublicKey().Bytes())
	sdir := filepath.Join(out, "sealed")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	fx.SealJobID, fx.SealType = "4b1d0c3e-7a52-4f0e-9c1a-5e0f00d1e5a1", "fs_extract"
	fx.SealParams = `{"project":"default","object":"volume/docs","snapshot":"","paths":["Q1","docs//2025/./x.txt"],"recipient_public_key":"` +
		base64.StdEncoding.EncodeToString(sk.PublicKey().Bytes()) + `"}`
	digest, err := jobspec.RequestDigest(fx.SealType, []byte(fx.SealParams))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"multi": rnd(2*MiB+12345, 11), "exact": rnd(MiB, 12), "small": []byte("restore report: ok\n"), "empty": nil} {
		var b bytes.Buffer
		w, err := format.NewSealWriter(&b, keys, sk.PublicKey().Bytes(), format.SealBinding{JobID: fx.SealJobID, Request: digest})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sdir, name+".sealed"), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sdir, name+".plain"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		fx.Sealed[name] = sealedOut{File: "sealed/" + name + ".sealed", Plain: "sealed/" + name + ".plain", Size: len(data)}
	}

	// an escrow blob of the master key (format.WrapMasterKey) and its offline private key
	epriv, epub, err := format.GenerateEscrowKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := format.WrapMasterKey(master, epub)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "escrow.bin"), wrapped, 0o644); err != nil {
		t.Fatal(err)
	}
	fx.EscrowFile, fx.EscrowPrivate = "escrow.bin", epriv

	j, _ := json.MarshalIndent(fx, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "fixture.json"), j, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture in %s: %d snapshots, %d chunks", out, len(fx.Snapshots), fx.Chunks)
}
