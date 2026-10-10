package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, y string) string {
	p := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(p, []byte(y), 0o600)
	return p
}

func TestLoadValidate(t *testing.T) {
	c, err := Load(write(t, "host: benfica\nservers:\n  cikarang: {address: \"b.example:8007\"}\n  amsterdam: {address: \"a.example:8007\", key_file: /k2.json}\ncontrol: cikarang\npolicies:\n  nightly: {schedule: \"03:00\", targets: [cikarang, amsterdam], commit_required: all}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.ControlServer() != "cikarang" || c.KeyFor("amsterdam") != "/k2.json" || c.KeyFor("cikarang") != c.KeyFile {
		t.Fatalf("%+v", c)
	}
	if got := c.ServerNames(); len(got) != 2 || got[0] != "amsterdam" {
		t.Fatal(got)
	}
}

func TestValidateRejects(t *testing.T) {
	for name, y := range map[string]string{
		"no servers":     "host: h\n",
		"bad control":    "host: h\nservers: {a: {address: x:1}}\ncontrol: b\n",
		"bad schedule":   "host: h\nservers: {a: {address: x:1}}\npolicies: {p: {schedule: \"99:99\"}}\n",
		"bad commit":     "host: h\nservers: {a: {address: x:1}}\npolicies: {p: {schedule: \"03:00\", commit_required: some}}\n",
		"unknown target": "host: h\nservers: {a: {address: x:1}}\npolicies: {p: {schedule: \"03:00\", targets: [b]}}\n",
		"no address":     "host: h\nservers: {a: {}}\n",
		"bad max_freeze": "host: h\nmax_freeze: soon\nservers: {a: {address: x:1}}\n",
	} {
		c, err := Load(write(t, y))
		if err == nil {
			err = c.Validate()
		}
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestShippedExampleLoadsAndValidates(t *testing.T) {
	c, err := Load("../../../examples/client.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the shipped example is invalid: %v", err)
	}
}

func TestFilesystemAndRecoverySettingsAreValidated(t *testing.T) {
	c := Defaults()
	c.Host = "h"
	c.Servers = map[string]Server{"a": {Address: "x:1"}}
	c.Filesystem.Format = "zip"
	if c.Validate() == nil {
		t.Fatal("an unknown filesystem format was accepted")
	}
	c.Filesystem.Format = "tar"
	c.Filesystem.FullEvery = "soon"
	if c.Validate() == nil {
		t.Fatal("a bad full_every was accepted")
	}
	c.Filesystem.FullEvery = "24h"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPerformanceSettingsAreValidated(t *testing.T) {
	base := "host: h\nservers: {a: {address: x:1}}\n"
	c, err := Load(write(t, base+"performance: {encode_workers: 6, upload_workers: 16}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil || c.Performance.EncodeWorkers != 6 || c.Performance.UploadWorkers != 16 {
		t.Fatalf("%v %+v", err, c.Performance)
	}
	for _, y := range []string{"performance: {encode_workers: -1}\n", "performance: {upload_workers: 1000}\n", "performance: {encode_workers: 65}\n"} {
		c, err := Load(write(t, base+y))
		if err != nil {
			t.Fatal(err)
		}
		if c.Validate() == nil {
			t.Fatalf("accepted %q", y)
		}
	}
	// absent: the engine defaults (0)
	c, _ = Load(write(t, base))
	if err := c.Validate(); err != nil || c.Performance != (Performance{}) {
		t.Fatalf("%v %+v", err, c.Performance)
	}
}
