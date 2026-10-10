package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"strings"

	"github.com/spf13/cobra"

	"github.com/unidoc/incus-backup/internal/client/config"
	"github.com/unidoc/incus-backup/internal/client/incusx"
	"github.com/unidoc/incus-backup/internal/nbd"
)

// cmdDebug holds hidden helpers used by the gate scripts (scripts/gates): they
// read and write volumes through the same Incus endpoints the engines use, so a
// gate can compare a restored volume with its source byte for byte.
func (c *cli) cmdDebug() *cobra.Command {
	root := &cobra.Command{Use: "debug", Short: "Helpers for the validation gates (hidden)", Hidden: true}
	var project, instance, volume string

	ref := func() (incusx.VolRef, error) {
		switch {
		case instance != "":
			return incusx.VolRef{Project: project, Type: "virtual-machine", Name: instance}, nil
		case volume != "":
			p := strings.SplitN(volume, "/", 2)
			if len(p) != 2 {
				return incusx.VolRef{}, fmt.Errorf("--volume must be POOL/NAME")
			}
			return incusx.VolRef{Pool: p[0], Project: project, Type: "custom", Name: p[1]}, nil
		}
		return incusx.VolRef{}, fmt.Errorf("give --instance NAME (a stopped VM's root disk) or --volume POOL/NAME")
	}
	open := func(cmd *cobra.Command, writable bool) (*nbd.Conn, error) {
		cfg, err := config.Load(c.cfgPath)
		if err != nil {
			return nil, err
		}
		inc, err := connectIncus(cfg.Incus.Socket)
		if err != nil {
			return nil, err
		}
		r, err := ref()
		if err != nil {
			return nil, err
		}
		if r.Type == "virtual-machine" {
			// the pool of the root disk
			insts, err := inc.Instances(cmd.Context(), project)
			if err != nil {
				return nil, err
			}
			for _, i := range insts {
				if i.Name == instance {
					r.Pool = i.RootPool()
				}
			}
		}
		nc, err := inc.VolumeNBD(cmd.Context(), r, writable)
		if err != nil {
			return nil, err
		}
		return nbd.Dial(cmd.Context(), nc, "", nil)
	}

	hash := &cobra.Command{Use: "hash", Short: "SHA-256 of a volume or stopped VM root disk, read through Incus NBD", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := open(cmd, false)
			if err != nil {
				return err
			}
			defer conn.Close()
			h := sha256.New()
			buf := make([]byte, 4<<20)
			for off := uint64(0); off < conn.Size(); {
				n := uint64(len(buf))
				if off+n > conn.Size() {
					n = conn.Size() - off
				}
				if _, err := conn.ReadAt(buf[:n], int64(off)); err != nil {
					return err
				}
				_, _ = h.Write(buf[:n])
				off += n
			}
			fmt.Printf("%s  %d bytes\n", hex.EncodeToString(h.Sum(nil)), conn.Size())
			return nil
		}}
	var seed int64
	var mib int
	write := &cobra.Command{Use: "write", Short: "Fill the first --mib MiB of a volume with deterministic pseudo-random data", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := open(cmd, true)
			if err != nil {
				return err
			}
			defer conn.Close()
			rng := rand.New(rand.NewSource(seed))
			buf := make([]byte, 1<<20)
			for i := 0; i < mib && uint64(i+1)<<20 <= conn.Size(); i++ {
				_, _ = io.ReadFull(rng, buf)
				if _, err := conn.WriteAt(buf, int64(i)<<20); err != nil {
					return err
				}
			}
			return conn.Flush()
		}}
	write.Flags().Int64Var(&seed, "seed", 1, "pseudo-random seed")
	write.Flags().IntVar(&mib, "mib", 16, "how many MiB to write")

	var snap string
	spath := &cobra.Command{Use: "snapshot-path", Short: "Show where the contents of an Incus snapshot can be read on this host", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(c.cfgPath)
			if err != nil {
				return err
			}
			inc, err := connectIncus(cfg.Incus.Socket)
			if err != nil {
				return err
			}
			var r incusx.VolRef
			if instance != "" {
				insts, err := inc.Instances(cmd.Context(), project)
				if err != nil {
					return err
				}
				for _, i := range insts {
					if i.Name == instance {
						r = incusx.RootRef(i)
					}
				}
			} else if r, err = ref(); err != nil {
				return err
			}
			p, release, err := inc.SnapshotPath(cmd.Context(), r, snap)
			if err != nil {
				return err
			}
			defer release()
			fmt.Println(p)
			return nil
		}}
	spath.Flags().StringVar(&snap, "snapshot", "", "snapshot name (as created with `incus snapshot create`)")
	for _, cmd := range []*cobra.Command{hash, write, spath} {
		cmd.Flags().StringVar(&project, "project", "default", "Incus project")
		cmd.Flags().StringVar(&instance, "instance", "", "instance name")
		cmd.Flags().StringVar(&volume, "volume", "", "POOL/NAME of a custom volume")
	}
	root.AddCommand(hash, write, spath)
	return root
}
