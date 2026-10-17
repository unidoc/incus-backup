// Command ibs3 serves the in-process S3 test server (internal/s3test/s3fake) on a TCP port: a
// hermetic S3 service for the Playwright e2e tests and for trying S3 repositories by hand. It is a
// development tool; it is never packaged and is not an object store (no multipart, no TLS).
//
//	ibs3 -addr 127.0.0.1:9000 -access K -secret S -bucket backups -dir /tmp/ibs3
//
// It prints "ibs3: listening on HOST:PORT" once it is ready and runs until SIGINT/SIGTERM.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/unidoc/incus-backup/internal/s3test/s3fake"
)

func main() {
	var opt s3fake.Options
	var buckets string
	flag.StringVar(&opt.Addr, "addr", "127.0.0.1:0", "listen address")
	flag.StringVar(&opt.Access, "access", s3fake.DefaultAccess, "access key")
	flag.StringVar(&opt.Secret, "secret", s3fake.DefaultSecret, "secret key")
	flag.StringVar(&opt.Region, "region", s3fake.DefaultRegion, "region")
	flag.StringVar(&opt.Dir, "dir", "", "keep the data in this directory (default: memory only)")
	flag.StringVar(&buckets, "bucket", "", "comma-separated buckets to create at start")
	flag.Parse()
	srv, err := s3fake.New(opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ibs3: %v\n", err)
		os.Exit(1)
	}
	for _, b := range strings.Split(buckets, ",") {
		if b = strings.TrimSpace(b); b != "" {
			if err := srv.EnsureBucket(b); err != nil {
				fmt.Fprintf(os.Stderr, "ibs3: %v\n", err)
				os.Exit(1)
			}
		}
	}
	fmt.Printf("ibs3: listening on %s\n", srv.Endpoint())
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	srv.Close()
}
