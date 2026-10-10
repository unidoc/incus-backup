package main

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/unidoc/incus-backup/internal/client/incusx"
)

var errNoIncus = errors.New("Incus is not reachable on this machine")

// noIncus stands in when only repository operations (list, verify, restore to a
// directory) are needed.
type noIncus struct{}

func (noIncus) Info(context.Context) (*incusx.Info, error)                   { return nil, errNoIncus }
func (noIncus) Projects(context.Context) ([]string, error)                   { return nil, errNoIncus }
func (noIncus) Instances(context.Context, string) ([]incusx.Instance, error) { return nil, errNoIncus }
func (noIncus) Volumes(context.Context, string) ([]incusx.Volume, error)     { return nil, errNoIncus }
func (noIncus) CreateInstanceBitmap(context.Context, string, string, string, bool) error {
	return errNoIncus
}
func (noIncus) CreateBitmap(context.Context, incusx.VolRef, string, bool) error { return errNoIncus }
func (noIncus) Bitmaps(context.Context, incusx.VolRef) ([]incusx.Bitmap, error) {
	return nil, errNoIncus
}
func (noIncus) DeleteBitmap(context.Context, incusx.VolRef, string) error { return errNoIncus }
func (noIncus) InstanceNBD(context.Context, string, string, bool) (net.Conn, error) {
	return nil, errNoIncus
}
func (noIncus) VolumeNBD(context.Context, incusx.VolRef, bool) (net.Conn, error) {
	return nil, errNoIncus
}
func (noIncus) CreateSnapshot(context.Context, incusx.VolRef, string) error { return errNoIncus }
func (noIncus) DeleteSnapshot(context.Context, incusx.VolRef, string) error { return errNoIncus }
func (noIncus) SnapshotPath(context.Context, incusx.VolRef, string) (string, func(), error) {
	return "", nil, errNoIncus
}
func (noIncus) FilesystemPath(context.Context, incusx.VolRef) (string, func(), error) {
	return "", nil, errNoIncus
}
func (noIncus) CreateInstance(context.Context, incusx.InstanceSpec) error { return errNoIncus }
func (noIncus) CreateVolume(context.Context, incusx.VolumeSpec) error     { return errNoIncus }
func (noIncus) StartInstance(context.Context, string, string) error       { return errNoIncus }
func (noIncus) StopInstance(context.Context, string, string) error        { return errNoIncus }
func (noIncus) InstanceExists(context.Context, string, string) (bool, error) {
	return false, errNoIncus
}
func (noIncus) VolumeExists(context.Context, incusx.VolRef) (bool, error) { return false, errNoIncus }

func (noIncus) CreateRecoveryVM(context.Context, incusx.RecoverySpec) error      { return errNoIncus }
func (noIncus) WaitStopped(context.Context, string, string, time.Duration) error { return errNoIncus }
func (noIncus) DeleteInstance(context.Context, string, string) error             { return errNoIncus }
func (noIncus) DeleteVolume(context.Context, incusx.VolRef) error                { return errNoIncus }
