package logring_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/unidoc/incus-backup/internal/server/logring"
)

func TestRingTeesFiltersRedactsAndBounds(t *testing.T) {
	r := logring.New(5)
	var out bytes.Buffer
	log := slog.New(logring.NewHandler(r, slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log.Debug("debug only in the ring")
	log.Info("setup", "setup_token", "ibs-SECRET-123", "addr", ":8443")
	log.With("repo", "custody").WithGroup("s3").Warn("slow", "secret_key", "S3SECRET", "ms", 900)
	log.Error("failed", "err", "disk full")
	// the real output still has everything it had before (the tee only redacts its own copy)
	if !strings.Contains(out.String(), "ibs-SECRET-123") || strings.Contains(out.String(), "debug only") {
		t.Fatalf("next handler output:\n%s", out.String())
	}
	all := r.Get(logring.Query{MinLevel: slog.LevelDebug})
	if len(all.Lines) != 3 || all.LastSeq != 3 { // debug is not captured (the daemon logs at info)
		t.Fatalf("%+v", all)
	}
	for _, l := range all.Lines {
		if strings.Contains(l.Attrs, "SECRET") {
			t.Fatalf("secret in the ring: %+v", l)
		}
	}
	if all.Lines[0].Attrs != `setup_token=[redacted] addr=:8443` || all.Lines[1].Attrs != `repo=custody s3.secret_key=[redacted] s3.ms=900` {
		t.Fatalf("attrs %q / %q", all.Lines[0].Attrs, all.Lines[1].Attrs)
	}
	warn := r.Get(logring.Query{MinLevel: slog.LevelWarn})
	if len(warn.Lines) != 2 || warn.Lines[0].Message != "slow" {
		t.Fatalf("warn+ %+v", warn.Lines)
	}
	if q := r.Get(logring.Query{Contains: "DISK"}); len(q.Lines) != 1 || q.Lines[0].Level != "ERROR" {
		t.Fatalf("search %+v", q.Lines)
	}
	if q := r.Get(logring.Query{After: 2}); len(q.Lines) != 1 || q.Lines[0].Seq != 3 {
		t.Fatalf("after %+v", q.Lines)
	}
	for i := 0; i < 10; i++ {
		log.Info("more")
	}
	b := r.Get(logring.Query{})
	if len(b.Lines) != 5 || b.Lines[0].Seq != 9 || b.Dropped != 8 || b.Capacity != 5 {
		t.Fatalf("bounded %+v", b)
	}
	if _, err := logring.ParseLevel("loud"); err == nil {
		t.Fatal("bad level accepted")
	}
}
