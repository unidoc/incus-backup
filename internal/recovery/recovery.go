// Package recovery defines how single files are recovered from a backed up VM disk without
// ever mounting the untrusted guest filesystem on the backup host.
//
// The disk is restored into a scratch block volume. A disposable VM with NO network, no profile
// devices and a hard memory limit is booted from a small image; a script we ship (guest.sh)
// mounts the guest disk read-only, writes a tar of the requested paths to a second scratch
// volume and powers the VM off. The host never mounts anything: it reads the output volume
// through Incus's NBD endpoint, parses a small header and extracts the tar with the hostile-input
// rules of archive.ExtractOptions.Untrusted. Whatever a malicious guest filesystem does, it can only
// influence bytes in a disposable VM and a tar stream we treat as hostile.
package recovery

import (
	"archive/tar"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

//go:embed guest.sh
var guestScript string

// GuestScript is the script that runs inside the recovery VM.
func GuestScript() string { return guestScript }

// HeaderSize is the size of the output header; the tar stream starts right after it.
const HeaderSize = 4096

const headerMagic = "IBREC1\n"

// Header is what the guest writes at the start of the output volume once it is done.
type Header struct {
	Status    string            `json:"status"` // "ok" or "error"
	Error     string            `json:"error,omitempty"`
	Partition string            `json:"partition,omitempty"`
	FS        string            `json:"fs,omitempty"`
	Paths     map[string]string `json:"paths,omitempty"` // requested path -> "ok" | "missing" | "symlink in path" | "not allowed"
	// PathsOmitted: the request was too long for the header; the archive is complete anyway.
	PathsOmitted bool   `json:"paths_omitted,omitempty"`
	Log          string `json:"log,omitempty"`
}

// Request is what the host asks the guest to recover.
type Request struct {
	Paths     []string // absolute paths inside the guest filesystem
	Partition int      // 0 = the first partition (or the whole disk) that holds the first path
}

// CleanPaths validates and normalises requested paths.
func CleanPaths(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("no paths requested")
	}
	var out []string
	for _, p := range in {
		if p == "" || strings.ContainsAny(p, "\x00\n\r") {
			return nil, fmt.Errorf("invalid path %q", p)
		}
		var parts []string
		for _, c := range strings.Split(p, "/") {
			switch c {
			case "", ".":
			case "..":
				if len(parts) == 0 {
					return nil, fmt.Errorf("path %q escapes the root", p)
				}
				parts = parts[:len(parts)-1]
			default:
				parts = append(parts, c)
			}
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("path %q is the whole filesystem: ask for specific files or directories", p)
		}
		out = append(out, "/"+strings.Join(parts, "/"))
	}
	return out, nil
}

// UserData renders the cloud-init user-data that makes the guest run the recovery script.
func UserData(r Request) (string, error) {
	paths, err := CleanPaths(r.Paths)
	if err != nil {
		return "", err
	}
	if r.Partition < 0 || r.Partition > 128 {
		return "", errors.New("invalid partition number")
	}
	var b strings.Builder
	b.WriteString("#cloud-config\nwrite_files:\n")
	b.WriteString("  - path: /etc/ibk-request\n    permissions: '0600'\n    content: |\n")
	fmt.Fprintf(&b, "      partition=%d\n", r.Partition)
	for _, p := range paths {
		fmt.Fprintf(&b, "      path=%s\n", p)
	}
	b.WriteString("  - path: /usr/local/sbin/ibk-recover\n    permissions: '0700'\n    content: |\n")
	for _, l := range strings.Split(strings.TrimRight(guestScript, "\n"), "\n") {
		b.WriteString("      " + l + "\n")
	}
	b.WriteString("runcmd:\n  - [ /usr/local/sbin/ibk-recover ]\n")
	return b.String(), nil
}

// ParseOutput reads the header from the output volume and returns a reader positioned at the tar
// stream. The tar is read lazily; ReadAt-style access is enough.
func ParseOutput(r io.ReaderAt) (*Header, *tar.Reader, error) {
	buf := make([]byte, HeaderSize)
	if _, err := r.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, nil, fmt.Errorf("reading the recovery output: %w", err)
	}
	if !bytes.HasPrefix(buf, []byte(headerMagic)) {
		return nil, nil, errors.New("the recovery VM wrote no result (it did not finish: check the image, cloud-init and the guest disk)")
	}
	body := bytes.TrimRight(buf[len(headerMagic):], "\x00 \n")
	var h Header
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, nil, fmt.Errorf("the recovery result is damaged: %w", err)
	}
	if h.Status != "ok" {
		msg := h.Error
		if msg == "" {
			msg = "unknown error"
		}
		return &h, nil, fmt.Errorf("the recovery VM reported an error: %s", msg)
	}
	return &h, tar.NewReader(&offsetReader{r: r, off: HeaderSize}), nil
}

type offsetReader struct {
	r   io.ReaderAt
	off int64
}

func (o *offsetReader) Read(p []byte) (int, error) {
	n, err := o.r.ReadAt(p, o.off)
	o.off += int64(n)
	if n > 0 && err == io.EOF {
		err = nil
	}
	return n, err
}

// EncodeHeader renders a header the way the guest script does (used by the fake guest in tests).
func EncodeHeader(h Header) []byte {
	b, _ := json.Marshal(h)
	out := make([]byte, HeaderSize)
	n := copy(out, headerMagic)
	copy(out[n:], b)
	return out
}
