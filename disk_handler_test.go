package main_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	zfs "github.com/bicomsystems/go-libzfs"
	"github.com/otiai10/copy"
	"gopkg.in/yaml.v2"
)

const mB = 1024 * 1024

type FakeDevices struct {
	Devices []FakeDevice
	*testing.T
}

type FakeDevice struct {
	Name string
	Type string
	ZFS  struct {
		PoolName string `yaml:"pool_name"`
		Datasets []struct {
			Name             string
			Content          string
			ZsysBootfs       bool      `yaml:"zsys_bootfs"`
			LastUsed         time.Time `yaml:"last_used"`
			LastBootedKernel string    `yaml:"last_booted_kernel"`
			Mountpoint       string
			CanMount         string
			Snapshots        []struct {
				Name             string
				Content          string
				CreationDate     time.Time `yaml:"creation_date"`
				LastBootedKernel string    `yaml:"last_booted_kernel"`
			}
		}
	}
}

// newFakeDevices returns a FakeDevices from a yaml file
func newFakeDevices(t *testing.T, path string) FakeDevices {
	devices := FakeDevices{T: t}
	b, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal("couldn't read yaml definition file", err)
	}
	if err = yaml.Unmarshal(b, &devices); err != nil {
		t.Fatal("couldn't unmarshal device list", err)
	}

	return devices
}

// create on disk mock devices as files
func (fdevice FakeDevices) create(path string) {
	for _, device := range fdevice.Devices {

		// Create file on disk
		p := filepath.Join(path, device.Name+".disk")
		f, err := os.Create(p)
		if err != nil {
			fdevice.Fatal("couldn't create device file on disk", err)
		}
		if err = f.Truncate(100 * mB); err != nil {
			f.Close()
			fdevice.Fatal("couldn't initializing device size on disk", err)
		}
		f.Close()

		switch strings.ToLower(device.Type) {
		case "zfs":
			poolMountPath := filepath.Join(path, device.Name)
			if err := os.MkdirAll(poolMountPath, os.ModeDir); err != nil {
				fdevice.Fatal("couldn't create directory for pool", err)
			}
			vdev := zfs.VDevTree{
				Type:    zfs.VDevTypeFile,
				Path:    p,
				Devices: []zfs.VDevTree{{Type: zfs.VDevTypeFile, Path: p}},
			}

			features := make(map[string]string)
			props := make(map[zfs.Prop]string)
			props[zfs.PoolPropAltroot] = poolMountPath
			fsprops := make(map[zfs.Prop]string)
			fsprops[zfs.DatasetPropMountpoint] = "/"
			fsprops[zfs.DatasetPropCanmount] = "off"

			pool, err := zfs.PoolCreate(device.ZFS.PoolName, vdev, features, props, fsprops)
			if err != nil {
				fdevice.Fatalf("couldn't create pool %q: %v", device.ZFS.PoolName, err)
			}
			defer pool.Close()
			defer pool.Export(true, "export temporary pool")

			for _, dataset := range device.ZFS.Datasets {
				func() {
					datasetName := device.ZFS.PoolName + "/" + dataset.Name
					datasetPath := ""
					shouldMount := false
					props := make(map[zfs.Prop]zfs.Property)
					if dataset.Mountpoint != "" {
						props[zfs.DatasetPropMountpoint] = zfs.Property{Value: dataset.Mountpoint}
						datasetPath = filepath.Join(poolMountPath, dataset.Mountpoint)
					}
					if dataset.CanMount != "" && dataset.Content != "" {
						props[zfs.DatasetPropCanmount] = zfs.Property{Value: dataset.CanMount}
						if dataset.CanMount == "noauto" || dataset.CanMount == "on" {
							shouldMount = true
						}
					}
					d, err := zfs.DatasetCreate(datasetName, zfs.DatasetTypeFilesystem, props)
					if err != nil {
						fdevice.Fatalf("couldn't create dataset %q: %v", datasetName, err)
					}
					defer d.Close()
					if dataset.ZsysBootfs {
						d.SetUserProperty("org.zsys:bootfs", "yes")
					}
					if dataset.LastBootedKernel != "" {
						d.SetUserProperty("org.zsys:last-booted-kernel", dataset.LastBootedKernel)
					}
					if !dataset.LastUsed.IsZero() {
						d.SetUserProperty("org.zsys:last-used", strconv.FormatInt(dataset.LastUsed.Unix(), 10))
					}
					if shouldMount {
						if err := d.Mount("", 0); err != nil {
							fdevice.Fatalf("couldn't mount dataset: %q: %v", datasetName, err)
						}
						defer os.RemoveAll(datasetPath)
						defer d.UnmountAll(0)
					}

					for _, s := range dataset.Snapshots {
						func() {
							replaceContent(fdevice.T, s.Content, datasetPath)
							props := make(map[zfs.Prop]zfs.Property)
							d, err := zfs.DatasetSnapshot(datasetName+"@"+s.Name, false, props)
							if err != nil {
								fmt.Fprintf(os.Stderr, "Couldn't create snapshot %q: %v\n", datasetName+"@"+s.Name, err)
								os.Exit(1)
							}
							defer d.Close()
							d.SetUserProperty("org.zsys:creation.test", strconv.FormatInt(s.CreationDate.Unix(), 10))
							if s.LastBootedKernel != "" {
								d.SetUserProperty("org.zsys:last-booted-kernel", s.LastBootedKernel)
							}
						}()
					}

					if shouldMount {
						replaceContent(fdevice.T, dataset.Content, datasetPath)
					}
				}()

			}

		case "ext4":
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", p)

			if err := cmd.Run(); err != nil {
				fdevice.Fatal("got error, expected none", err)
			}

		case "":
			// do nothing for "no pool, no partition" (empty disk)

		default:
			fdevice.Fatalf("unknown type: %s", device.Type)
		}
	}

}

// replaceContent replaces content in dst from src content (preserving src)
func replaceContent(t *testing.T, src, dst string) {
	entries, err := ioutil.ReadDir(dst)
	if err != nil {
		t.Fatalf("couldn't read directory content for %q: %v", dst, err)
	}
	for _, e := range entries {
		p := filepath.Join(dst, e.Name())
		if err := os.RemoveAll(p); err != nil {
			t.Fatalf("couldn't clean up %q: %v", p, err)
		}
	}

	if err := copy.Copy(src, dst); err != nil {
		t.Fatalf("couldn't copy %q to %q: %v", src, dst, err)
	}
}