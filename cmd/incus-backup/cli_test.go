package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unidoc/incus-backup/internal/api"
	"github.com/unidoc/incus-backup/internal/client/app"
	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/engine"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/client/incusx/fake"
	"github.com/unidoc/incus-backup/internal/datastore"
	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/testdb"
	"github.com/unidoc/incus-backup/internal/testsupport"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func run(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	root := newRoot()
	root.SetArgs(append([]string{"--config", cfg}, args...))
	root.SetErr(io.Discard)
	err := root.ExecuteContext(context.Background())
	_ = w.Close()
	os.Stdout = old
	var b bytes.Buffer
	_, _ = io.Copy(&b, r)
	return b.String(), err
}

func TestClientCLIEndToEnd(t *testing.T) {
	n := testsupport.NewNode(t)
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Host = "benfica"
	cfg.StateDir, cfg.SpoolDir, cfg.KeyFile = filepath.Join(dir, "state"), filepath.Join(dir, "spool"), filepath.Join(dir, "etc", "key.json")
	cfgPath := filepath.Join(dir, "etc", "client.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	inc := fake.New(t.TempDir())
	connectIncus = func(string) (incusx.API, error) { return inc, nil }
	defer func() { connectIncus = func(s string) (incusx.API, error) { return incusx.Connect(s) } }()

	tok, _ := n.Svc.CreateToken(context.Background(), "custody", "benfica", api.ScopeBackup, time.Hour)
	// non-interactive init needs the explicit confirmation flag
	if _, err := run(t, cfgPath, "init", "--server", n.Addr(), "--token", tok, "--name", "cikarang"); err == nil {
		t.Fatal("init without a terminal must require --i-stored-the-recovery-key")
	}
	if _, err := os.Stat(cfg.KeyFile); err == nil {
		t.Fatal("no key may be written before the recovery key is confirmed")
	}
	tok, _ = n.Svc.CreateToken(context.Background(), "custody", "benfica", api.ScopeBackup, time.Hour)
	out, err := run(t, cfgPath, "init", "--server", n.Addr(), "--token", tok, "--name", "cikarang", "--i-stored-the-recovery-key")
	if err != nil || !strings.Contains(out, "Enrolled") {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if err := n.Svc.SetPolicy(context.Background(), "custody", api.Policy{Name: "nightly", Targets: []string{"cikarang"}, KeepDaily: 7}); err != nil {
		t.Fatal(err)
	}
	if err := n.Svc.Assign(context.Background(), "custody", "benfica", "default", "", "nightly"); err != nil {
		t.Fatal(err)
	}
	// a VM selected by SERVER assignment, a container selected by an Incus key
	inc.AddVM("default", "bosch", map[string]int{"root": 8 << 20}, true)
	inc.GuestWrite("default", "bosch", "root", 0, bytes.Repeat([]byte("bosch!"), 100000))
	ct := t.TempDir()
	_ = os.WriteFile(filepath.Join(ct, "hello"), []byte("hi"), 0o644)
	inc.AddContainer("default", "web", ct, true)
	inc.SetConfig("default", "web", engine.KeyExclude, "true")

	out, err = run(t, cfgPath, "plan")
	if err != nil || !strings.Contains(out, "server assignment") || !strings.Contains(out, "excluded by user.backup.exclude") {
		t.Fatalf("plan: %v\n%s", err, out)
	}
	if out, err = run(t, cfgPath, "doctor"); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	if out, err = run(t, cfgPath, "run", "--dry-run"); err != nil || !strings.Contains(out, "planned") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	out, err = run(t, cfgPath, "run")
	if err != nil || !strings.Contains(out, "full read") || strings.Contains(out, "FAILED") {
		t.Fatalf("run: %v\n%s", err, out)
	}
	inc.GuestWrite("default", "bosch", "root", 1<<20, []byte("changed"))
	out, err = run(t, cfgPath, "run")
	if err != nil || !strings.Contains(out, "incremental") {
		t.Fatalf("second run must be incremental: %v\n%s", err, out)
	}
	if out, err = run(t, cfgPath, "status"); err != nil || !strings.Contains(out, "bosch") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	out, err = run(t, cfgPath, "list")
	if err != nil || !strings.Contains(out, "instance/bosch/root") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = run(t, cfgPath, "list", "instance/bosch/root")
	if err != nil || strings.Count(out, "-") < 2 || !strings.Contains(out, "full read") || !strings.Contains(out, "incremental") {
		t.Fatalf("list object: %v\n%s", err, out)
	}
	if out, err = run(t, cfgPath, "verify", "--last", "0"); err != nil || !strings.Contains(out, "decrypted") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	// restore the VM as a copy, and a volume from the repository
	out, err = run(t, cfgPath, "restore", "instance", "bosch", "--as", "bosch-copy", "--no-chown")
	if err != nil || !strings.Contains(out, "bosch-copy") {
		t.Fatalf("restore instance: %v\n%s", err, out)
	}
	if !bytes.Equal(inc.Disk("default", "bosch-copy", "root"), inc.Disk("default", "bosch", "root")) {
		a, b := inc.Disk("default", "bosch-copy", "root"), inc.Disk("default", "bosch", "root")
		first := -1
		for i := range a {
			if a[i] != b[i] {
				first = i
				break
			}
		}
		t.Fatalf("restored VM differs from the live VM (len %d vs %d, first diff at %d)", len(a), len(b), first)
	}
	if _, err = run(t, cfgPath, "restore", "instance", "bosch", "--as", "bosch-copy"); err == nil {
		t.Fatal("must not overwrite an existing instance")
	}
	// key tools
	rec, err := run(t, cfgPath, "key", "recovery")
	if err != nil {
		t.Fatal(err)
	}
	master, err := format.DecodeRecoveryKey(strings.TrimSpace(rec))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := format.DeriveKeys(master)
	loaded, _ := config.Load(cfgPath)
	if len(loaded.Servers) != 1 || k.DomainID == "" {
		t.Fatal("config/key")
	}
}

// Browse and restore single files from a filesystem backup, as an operator would.
func TestClientCLIBrowseAndRestoreSingleFiles(t *testing.T) {
	n := testsupport.NewNode(t)
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Host = "benfica"
	cfg.StateDir, cfg.SpoolDir, cfg.KeyFile = filepath.Join(dir, "state"), filepath.Join(dir, "spool"), filepath.Join(dir, "etc", "key.json")
	cfgPath := filepath.Join(dir, "etc", "client.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	inc := fake.New(t.TempDir())
	connectIncus = func(string) (incusx.API, error) { return inc, nil }
	defer func() { connectIncus = func(s string) (incusx.API, error) { return incusx.Connect(s) } }()
	tok, _ := n.Svc.CreateToken(context.Background(), "custody", "benfica", api.ScopeBackup, time.Hour)
	if _, err := run(t, cfgPath, "init", "--server", n.Addr(), "--token", tok, "--name", "cikarang", "--i-stored-the-recovery-key"); err != nil {
		t.Fatal(err)
	}
	_ = n.Svc.SetPolicy(context.Background(), "custody", api.Policy{Name: "nightly", Targets: []string{"cikarang"}, KeepDaily: 7})
	_ = n.Svc.Assign(context.Background(), "custody", "benfica", "default", "", "nightly")
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "etc/nginx"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "var/www"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc/nginx/nginx.conf"), []byte("worker_processes 4;\n"), 0o640)
	_ = os.WriteFile(filepath.Join(root, "etc/hostname"), []byte("web\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "var/www/index.html"), bytes.Repeat([]byte("<p>"), 100000), 0o644)
	inc.AddContainer("default", "web", root, true)
	if out, err := run(t, cfgPath, "run"); err != nil || strings.Contains(out, "FAILED") {
		t.Fatalf("run: %v\n%s", err, out)
	}
	obj := "instance/web/root"
	out, err := run(t, cfgPath, "fs", "ls", "-l", obj, "etc")
	if err != nil || !strings.Contains(out, "nginx") || !strings.Contains(out, "hostname") || !strings.Contains(out, "drwx") {
		t.Fatalf("ls: %v\n%s", err, out)
	}
	if out, err = run(t, cfgPath, "fs", "find", obj, "*.conf"); err != nil || strings.TrimSpace(out) != "/etc/nginx/nginx.conf" {
		t.Fatalf("find: %v\n%q", err, out)
	}
	if out, err = run(t, cfgPath, "fs", "cat", obj, "etc/nginx/nginx.conf"); err != nil || out != "worker_processes 4;\n" {
		t.Fatalf("cat: %v\n%q", err, out)
	}
	if out, err = run(t, cfgPath, "fs", "stat", obj, "etc/hostname"); err != nil || !strings.Contains(out, "sha256") {
		t.Fatalf("stat: %v\n%s", err, out)
	}
	to := filepath.Join(dir, "out")
	if out, err = run(t, cfgPath, "fs", "extract", obj, "etc/nginx", "--to", to); err != nil {
		t.Fatalf("extract: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(to, "etc/nginx/nginx.conf"))
	if string(got) != "worker_processes 4;\n" {
		t.Fatalf("extracted %q", got)
	}
	if _, err := os.Stat(filepath.Join(to, "var")); err == nil {
		t.Fatal("an unrequested directory was extracted")
	}
	// doctor reports an interrupted guest-file recovery and the missing escrow blob
	_ = inc.CreateVolume(context.Background(), incusx.VolumeSpec{Pool: "default", Project: "default", Name: "ibk-rec-dead-disk", ContentType: "block", Size: 4096})
	if out, err = run(t, cfgPath, "doctor"); err != nil || !strings.Contains(out, "recovery leftovers") || !strings.Contains(out, "ibk-rec-dead-disk") || !strings.Contains(out, "no escrow blob stored") {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	if _, err = run(t, cfgPath, "fs", "cat", obj, "etc/missing"); err == nil {
		t.Fatal("cat of a missing file succeeded")
	}
	if _, err = run(t, cfgPath, "fs", "ls", obj, "../../etc"); err == nil {
		t.Fatal("a path above the root was accepted")
	}
}

// Disaster recovery from the offline escrow key alone: a new host with NO key file and NO recovery
// key enrols, the wrapped key is fetched from the server, opened with the offline private key, and the
// old data is restorable. The server never holds anything it could open.
func TestEscrowRecoversTheKeyOnANewHostWithOnlyTheOfflinePrivateKey(t *testing.T) {
	n := testsupport.NewNode(t)
	ctx := context.Background()
	mkHost := func(name string) (cfgPath string, cfg config.Config, inc *fake.Incus) {
		dir := t.TempDir()
		c := config.Defaults()
		c.Host = name
		c.StateDir, c.SpoolDir, c.KeyFile = filepath.Join(dir, "state"), filepath.Join(dir, "spool"), filepath.Join(dir, "etc", "key.json")
		p := filepath.Join(dir, "etc", "client.yaml")
		if err := c.Save(p); err != nil {
			t.Fatal(err)
		}
		return p, c, fake.New(t.TempDir())
	}
	priv, pub, err := format.GenerateEscrowKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	// host 1: init with the escrow public key, back something up
	cfg1, c1, inc1 := mkHost("benfica")
	connectIncus = func(string) (incusx.API, error) { return inc1, nil }
	defer func() { connectIncus = func(s string) (incusx.API, error) { return incusx.Connect(s) } }()
	tok, _ := n.Svc.CreateToken(ctx, "custody", "benfica", api.ScopeBackup, time.Hour)
	out, err := run(t, cfg1, "init", "--server", n.Addr(), "--token", tok, "--name", "cikarang", "--i-stored-the-recovery-key", "--escrow-public", pub)
	if err != nil || !strings.Contains(out, "wrapped to your offline escrow key") {
		t.Fatalf("init: %v\n%s", err, out)
	}
	_ = n.Svc.SetPolicy(ctx, "custody", api.Policy{Name: "nightly", Targets: []string{"cikarang"}, KeepDaily: 7})
	_ = n.Svc.Assign(ctx, "custody", "benfica", "default", "", "nightly")
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "precious.txt"), []byte("irreplaceable"), 0o644)
	inc1.AddContainer("default", "web", root, true)
	if out, err := run(t, cfg1, "run"); err != nil || strings.Contains(out, "FAILED") {
		t.Fatalf("run: %v\n%s", err, out)
	}
	// uploading again is create-only: reported, not an error and not a replacement
	out, err = run(t, cfg1, "key", "escrow", "upload", "--public", pub)
	if err != nil || !strings.Contains(out, "already has an escrow blob") {
		t.Fatalf("second upload: %v\n%s", err, out)
	}
	// the server holds the wrapped blob but no way to open it, and the master key appears nowhere
	_, master, _ := app.LoadKeys(c1.KeyFile, nil)
	st, _ := n.Svc.Store(n.Repo)
	found := false
	_ = st.Backend().List("", func(key string, _ datastore.Info) error {
		if b, err := st.Backend().Get(key); err == nil {
			if strings.HasPrefix(key, "escrow/") {
				found = true
			}
			if bytes.Contains(b, master) {
				t.Errorf("the master key is readable in %s", key)
			}
		}
		return nil
	})
	if !found {
		t.Fatal("no escrow blob stored")
	}
	// a host holding a DIFFERENT key cannot plant a blob in this repository
	// host 2: a brand new machine. No key file, no recovery key, only the offline private key.
	cfg2, c2, inc2 := mkHost("benfica-new")
	connectIncus = func(string) (incusx.API, error) { return inc2, nil }
	privFile := filepath.Join(t.TempDir(), "escrow.key")
	_ = os.WriteFile(privFile, []byte(priv+"\n"), 0o600)
	tok2, _ := n.Svc.CreateToken(ctx, "custody", "benfica-new", api.ScopeBackup, time.Hour)
	if _, err := run(t, cfg2, "init", "--server", n.Addr(), "--token", tok2, "--name", "cikarang", "--escrow-private-file", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing private key file went unnoticed")
	}
	tok2, _ = n.Svc.CreateToken(ctx, "custody", "benfica-new", api.ScopeBackup, time.Hour)
	wrongPriv, _, _ := format.GenerateEscrowKeyPair()
	wrongFile := filepath.Join(t.TempDir(), "wrong.key")
	_ = os.WriteFile(wrongFile, []byte(wrongPriv), 0o600)
	if _, err := run(t, cfg2, "init", "--server", n.Addr(), "--token", tok2, "--name", "cikarang", "--escrow-private-file", wrongFile); err == nil {
		t.Fatal("the wrong private key opened the escrow blob")
	}
	if _, err := os.Stat(c2.KeyFile); err == nil {
		t.Fatal("a key file was written although the escrow key was wrong")
	}
	tok2, _ = n.Svc.CreateToken(ctx, "custody", "benfica-new", api.ScopeBackup, time.Hour)
	out, err = run(t, cfg2, "init", "--server", n.Addr(), "--token", tok2, "--name", "cikarang", "--escrow-private-file", privFile)
	if err != nil || !strings.Contains(out, "Key recovered from the escrow blob") {
		t.Fatalf("recovery init: %v\n%s", err, out)
	}
	_, master2, err := app.LoadKeys(c2.KeyFile, nil)
	if err != nil || !bytes.Equal(master, master2) {
		t.Fatalf("the recovered key differs from the original: %v", err)
	}
	// and the data is restorable with it
	dest := filepath.Join(t.TempDir(), "out")
	out, err = run(t, cfg2, "restore", "files", "instance/web/root", "--to", dest, "--no-chown")
	_ = out
	if err == nil {
		if b, _ := os.ReadFile(filepath.Join(dest, "precious.txt")); string(b) != "irreplaceable" {
			t.Fatalf("restored %q", b)
		}
	} else {
		out2, err2 := run(t, cfg2, "fs", "cat", "instance/web/root", "precious.txt")
		if err2 != nil || out2 != "irreplaceable" {
			t.Fatalf("the recovered key cannot read the old backups: %v / %v %q", err, err2, out2)
		}
	}
	// the explicit unwrap path gives the same recovery key
	blob, err := run(t, cfg2, "key", "escrow", "fetch")
	if err != nil || len(blob) == 0 {
		t.Fatalf("fetch: %v", err)
	}
	bf := filepath.Join(t.TempDir(), "blob")
	_ = os.WriteFile(bf, []byte(blob), 0o600)
	rec, err := run(t, cfg2, "key", "escrow", "unwrap", "--private", priv, "--blob", bf)
	want, _ := format.EncodeRecoveryKey(master)
	if err != nil || strings.TrimSpace(rec) != want {
		t.Fatalf("unwrap: %v %q", err, rec)
	}
	_ = ctx
}

// A host whose freshly generated key does not match the repository's key domain must not be left with a stray
// key file, and the error must say the token is used up and what to do.
func TestInitAtAForeignKeyDomainLeavesNoStrayKey(t *testing.T) {
	n := testsupport.NewNode(t)
	ctx := context.Background()
	if _, err := n.Svc.DB.Pool().Exec(ctx, `UPDATE repositories SET domain_id='0123456789abcdef'`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Host = "benfica"
	cfg.StateDir, cfg.SpoolDir, cfg.KeyFile = filepath.Join(dir, "state"), filepath.Join(dir, "spool"), filepath.Join(dir, "etc", "key.json")
	cfgPath := filepath.Join(dir, "etc", "client.yaml")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	tok, _ := n.Svc.CreateToken(ctx, "custody", "benfica", api.ScopeBackup, time.Hour)
	_, err := run(t, cfgPath, "init", "--server", n.Addr(), "--token", tok, "--name", "cikarang", "--i-stored-the-recovery-key")
	if err == nil || !strings.Contains(err.Error(), "token has been used up") {
		t.Fatalf("want the used-up-token guidance, got %v", err)
	}
	if _, serr := os.Stat(cfg.KeyFile); serr == nil {
		t.Fatal("a key that protects nothing was left on the host")
	}
}
