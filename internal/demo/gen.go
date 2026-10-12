package demo

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Data generators: deterministic (seeded) content with a realistic mix of compressible text and
// incompressible bytes, so the stored chunks show real compression and deduplication numbers.

var words = strings.Fields(`the backup server repository snapshot chunk index manifest volume instance project policy
retention daily weekly hourly verify restore database postgres nginx request response client host
error warning info debug started finished upload download session commit domain key encrypted
user admin operator customer invoice order payment shipment warehouse ledger account report quarter
GET POST PUT DELETE /api/v1/orders /api/v1/users /healthz 200 201 204 304 400 401 403 404 500 502`)

// textBytes returns n bytes of log-like text (compresses roughly 4-6x with zstd).
func textBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	var b strings.Builder
	b.Grow(n + 256)
	t := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seed%1000) * time.Hour)
	for b.Len() < n {
		t = t.Add(time.Duration(r.Intn(40)) * 100 * time.Millisecond)
		fmt.Fprintf(&b, "%s host-%02d app[%d]: ", t.Format("2006-01-02T15:04:05.000Z"), r.Intn(12), 1000+r.Intn(50))
		for i, k := 0, 4+r.Intn(10); i < k; i++ {
			b.WriteString(words[r.Intn(len(words))])
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "id=%d dur=%dms\n", 4000+r.Intn(500), 10*r.Intn(90))
	}
	return []byte(b.String()[:n])
}

// fileWord is a random word usable in a file name.
func fileWord(r *rand.Rand) string {
	return strings.Trim(strings.ReplaceAll(words[r.Intn(len(words))], "/", "_"), "_")
}

// randBytes returns n incompressible bytes.
func randBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// TreeSpec sizes a generated file tree.
type TreeSpec struct {
	Files     int     // regular files (approximately)
	LargeMiB  int     // total size of the few large binary files
	SmallKiB  int     // typical small file size
	Seed      int64   //
	BinaryPct float64 // share of small files that are binary
}

// unicode and awkward names a file browser must render and escape correctly
var oddNames = []string{
	"Ársskýrsla 2025.pdf", "Þórsmörk-ferð.txt", "Ölgerðin pöntun #42.csv", "日本語のメモ.md", "Ελληνικά σημειώσεις.txt",
	"Résumé – final (v3).docx", "emoji 🎉 party.txt", "Кириллица.odt", "spaces   in   name.txt", "UPPER and lower.TXT",
	"name with 'quotes' and \"double\".txt", "semi;colon&ampersand.txt", "100% done.txt", "-leading-dash.txt",
	"a_very_long_file_name_that_goes_on_and_on_to_test_truncation_in_the_file_browser_column_widths.log",
}

var dirNames = []string{"Documents", "Projects", "Invoices", "Photos", "Archive", "Shared", "Templates", "Reports", "Contracts",
	"Marketing", "Engineering", "HR", "Finance", "2023", "2024", "2025", "2026", "Q1", "Q2", "Q3", "Q4", "drafts", "old", "Kynningar",
	"Fundargerðir", "Ársuppgjör", "クライアント", "Λογιστήριο"}

var exts = []string{".txt", ".md", ".csv", ".json", ".yaml", ".log", ".html", ".odt", ".pdf", ".png", ".jpg", ".bin", ".sql", ".xml"}

// WriteTree creates a nested tree of about spec.Files files under root: text and binary files of many
// sizes, a few large binaries, empty files and directories, hidden files, symlinks (relative,
// absolute, dangling) and unicode names.
func WriteTree(root string, spec TreeSpec) error {
	r := rand.New(rand.NewSource(spec.Seed))
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// directories: up to 6 levels deep
	dirs := []string{""}
	for len(dirs) < spec.Files/12+4 {
		parent := dirs[r.Intn(len(dirs))]
		if strings.Count(parent, "/") >= 5 {
			continue
		}
		d := filepath.Join(parent, dirNames[r.Intn(len(dirNames))])
		if r.Intn(4) == 0 {
			d += fmt.Sprintf("-%d", r.Intn(100))
		}
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
		dirs = append(dirs, d)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty-folder", "also empty"), 0o755); err != nil {
		return err
	}
	var files []string
	write := func(rel string, b []byte, mode os.FileMode) error {
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, b, mode); err != nil {
			return err
		}
		files = append(files, rel)
		// spread modification times over the last two years
		mt := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC).Add(-time.Duration(r.Intn(730*24)) * time.Hour)
		return os.Chtimes(p, mt, mt)
	}
	for i, n := range oddNames {
		d := dirs[r.Intn(len(dirs))]
		if err := write(filepath.Join(d, n), textBytes(200+r.Intn(4000), spec.Seed+int64(i)), 0o644); err != nil {
			return err
		}
	}
	for i := 0; i < spec.Files; i++ {
		d := dirs[r.Intn(len(dirs))]
		ext := exts[r.Intn(len(exts))]
		name := fmt.Sprintf("%s-%04d%s", fileWord(r), i, ext)
		if r.Intn(25) == 0 {
			name = "." + name // hidden
		}
		size := r.Intn(spec.SmallKiB*1024*2) + 1
		switch {
		case r.Intn(40) == 0:
			size = 0
		case r.Intn(40) == 0:
			size = 64<<10 + r.Intn(448<<10)
		}
		var b []byte
		if ext == ".png" || ext == ".jpg" || ext == ".bin" || r.Float64() < spec.BinaryPct {
			b = randBytes(size, spec.Seed*7919+int64(i))
		} else {
			b = textBytes(size, spec.Seed*104729+int64(i))
		}
		mode := os.FileMode(0o644)
		if r.Intn(30) == 0 {
			mode = 0o755
		} else if r.Intn(30) == 0 {
			mode = 0o600
		}
		if err := write(filepath.Join(d, name), b, mode); err != nil {
			return err
		}
	}
	// a few large binaries ("videos", "disk images")
	if spec.LargeMiB > 0 {
		left := spec.LargeMiB << 20
		for i := 0; left > 0; i++ {
			n := min(left, (2+r.Intn(6))<<20)
			left -= n
			if err := write(filepath.Join(dirs[r.Intn(len(dirs))], fmt.Sprintf("recording-%02d.mp4", i)), randBytes(n, spec.Seed*31+int64(i)), 0o644); err != nil {
				return err
			}
		}
	}
	// symlinks: relative, absolute (inside the guest's view) and dangling
	if len(files) > 3 {
		_ = os.Symlink(files[0], filepath.Join(root, "latest-report"))
		_ = os.Symlink("/etc/hostname", filepath.Join(root, "absolute-link"))
		_ = os.Symlink("does/not/exist", filepath.Join(root, "dangling-link"))
		_ = os.Symlink(filepath.Dir(files[1]), filepath.Join(root, "shortcut to folder"))
	}
	return nil
}

// MutateTree simulates a day of use: some files change, a few are added, one or two are deleted.
func MutateTree(root string, day int, seed int64, changes int) error {
	r := rand.New(rand.NewSource(seed*1000 + int64(day)))
	var files []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		return nil
	}
	for i := 0; i < changes; i++ {
		p := files[r.Intn(len(files))]
		if strings.HasSuffix(p, ".mp4") {
			continue // large binaries are written once
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			continue
		}
		_, _ = f.Write(textBytes(200+r.Intn(3000), seed+int64(day*100+i)))
		_ = f.Close()
	}
	for i := 0; i < 1+changes/3; i++ {
		d := filepath.Dir(files[r.Intn(len(files))])
		p := filepath.Join(d, fmt.Sprintf("new-%s-day%02d-%d.txt", fileWord(r), day, i))
		if err := os.WriteFile(p, textBytes(500+r.Intn(20000), seed+int64(day*1000+i)), 0o644); err != nil {
			return err
		}
	}
	if day%3 == 0 {
		p := files[r.Intn(len(files))]
		if !strings.HasSuffix(p, ".mp4") {
			_ = os.Remove(p)
		}
	}
	return nil
}

// WriteContainerRoot creates a small Linux-like root filesystem.
func WriteContainerRoot(root, hostname string, files int, seed int64) error {
	base := map[string]string{
		"etc/hostname":                   hostname + "\n",
		"etc/hosts":                      "127.0.0.1 localhost\n::1 localhost\n127.0.1.1 " + hostname + "\n",
		"etc/os-release":                 "NAME=\"Alpine Linux\"\nID=alpine\nVERSION_ID=3.22.1\nPRETTY_NAME=\"Alpine Linux v3.22\"\n",
		"etc/passwd":                     "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/:/sbin/nologin\napp:x:1000:1000::/home/app:/bin/sh\n",
		"etc/apk/world":                  "alpine-base\nopenrc\n" + hostname + "\n",
		"etc/conf.d/" + hostname:         "command_args=\"--config /etc/" + hostname + ".conf\"\n",
		"root/.ash_history":              "apk update\napk upgrade\nrc-service " + hostname + " restart\n",
		"home/app/.profile":              "export PATH=$PATH:/home/app/bin\n",
		"var/lib/" + hostname + "/.keep": "",
	}
	for p, c := range base {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			return err
		}
	}
	_ = os.MkdirAll(filepath.Join(root, "tmp"), 0o1777)
	_ = os.Symlink("/usr/bin/busybox", filepath.Join(root, "bin-sh-link"))
	r := rand.New(rand.NewSource(seed))
	for i := 0; i < files; i++ {
		var p string
		var b []byte
		switch i % 4 {
		case 0:
			p = fmt.Sprintf("usr/bin/tool-%03d", i)
			b = randBytes(4<<10+r.Intn(60<<10), seed+int64(i))
		case 1:
			p = fmt.Sprintf("usr/share/doc/pkg-%03d/README", i)
			b = textBytes(1<<10+r.Intn(8<<10), seed+int64(i))
		case 2:
			p = fmt.Sprintf("var/log/%s/%s.log.%d", hostname, hostname, i)
			b = textBytes(4<<10+r.Intn(64<<10), seed+int64(i))
		default:
			p = fmt.Sprintf("etc/%s.d/%02d-conf-%d.conf", hostname, i%100, i)
			b = textBytes(200+r.Intn(2<<10), seed+int64(i))
		}
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(p, "usr/bin/") {
			mode = 0o755
		}
		if err := os.WriteFile(full, b, mode); err != nil {
			return err
		}
	}
	return nil
}
