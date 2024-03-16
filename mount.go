package gokrazy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/gokrazy/internal/rootdev"
)

const (
	additionalDisksFile = "mount-disks.json"
)

type additionalDisks struct {
	Disks []diskMount `json:"disks"`
}

type diskMount struct {
	PartUUID   string `json:"partUUID"`
	Type       string `json:"type"`
	Mountpoint string `json:"mountpoint"`
	Options    string `json:"options"`
}

func (d diskMount) validate() error {
	if d.PartUUID == "" {
		return errors.New("partUUID is needed to mount the disk")
	}
	if d.Type == "" {
		return errors.New("type is needed to mount disk")
	}
	if d.Mountpoint == "" {
		return errors.New("no mountpoint set")
	}
	return nil
}

func (d diskMount) mount() error {
	path, err := findBlockDevice(d.PartUUID)
	if err != nil {
		return err
	}
	return syscall.Mount(path, d.Mountpoint, d.Type, 0, d.Options)
}

// findBlockDevice finds the block device for the given partition UUID
func findBlockDevice(partUUID string) (string, error) {
	logError := func(e error) {
		log.Printf("error when searching for partition with ID %s: %v", partUUID, e)
	}
	var dev string
	err := filepath.WalkDir("/sys/block", func(path string, info fs.DirEntry, err error) error {
		if err != nil {
			logError(err)
			return nil
		}
		i, err := info.Info()
		if err != nil {
			logError(err)
			return nil
		}
		// we are only interested in symlinks
		if i.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		devname := filepath.Join("/dev", filepath.Base(path))
		disk, err := diskfs.Open(devname, diskfs.WithOpenMode(diskfs.ReadOnly))
		if err != nil {
			logError(err)
			return nil
		}
		partTable, err := disk.GetPartitionTable()
		if err != nil {
			logError(err)
			return nil
		}
		for i, part := range partTable.GetPartitions() {
			if strings.ToLower(part.UUID()) == strings.ToLower(partUUID) {
				dev = fmt.Sprintf("%s%d", devname, i+1)
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if dev == "" {
		return "", fmt.Errorf("could not find partition with UUID %s", partUUID)
	}
	return dev, nil
}

// mountCompat deals with old FAT root file systems, to cover the case where
// users use an old gokr-packer with a new github.com/gokrazy/gokrazy package.
func mountCompat() error {
	// Symlink /etc/resolv.conf. We cannot do this in the root file
	// system itself, as FAT does not support symlinks.
	if err := syscall.Mount("tmpfs", "/etc", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_RELATIME, "size=1M"); err != nil {
		return fmt.Errorf("tmpfs on /etc: %v", err)
	}

	if err := os.Symlink("/proc/net/pnp", "/etc/resolv.conf"); err != nil {
		return fmt.Errorf("etc: %v", err)
	}

	// Symlink /etc/localtime. We cannot do this in the root file
	// system, as FAT filenames are limited to 8.3.
	if err := os.Symlink("/localtim", "/etc/localtime"); err != nil {
		return fmt.Errorf("etc: %v", err)
	}

	if err := os.Mkdir("/etc/ssl", 0755); err != nil {
		return fmt.Errorf("/etc/ssl: %v", err)
	}

	if err := os.Symlink("/cacerts", "/etc/ssl/ca-bundle.pem"); err != nil {
		return fmt.Errorf("/etc/ssl: %v", err)
	}

	if err := ioutil.WriteFile("/etc/hosts", []byte("127.0.0.1 localhost\n::1 localhost\n"), 0644); err != nil {
		return fmt.Errorf("/etc/hosts: %v", err)
	}
	return nil
}

func mountfs() error {
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_RELATIME, ""); err != nil {
		return fmt.Errorf("tmpfs on /tmp: %v", err)
	}

	if err := os.Symlink("/proc/net/pnp", "/tmp/resolv.conf"); err != nil {
		return fmt.Errorf("etc: %v", err)
	}

	if _, err := os.Lstat("/etc/resolv.conf"); err != nil && os.IsNotExist(err) {
		if err := mountCompat(); err != nil {
			return err
		}
	}

	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /dev was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("devtmpfs: %v", err)
		}
	}

	if err := os.MkdirAll("/dev/pts", 0755); err != nil {
		return fmt.Errorf("mkdir /dev/pts: %v", err)
	}

	if err := syscall.Mount("devpts", "/dev/pts", "devpts", 0, ""); err != nil {
		return fmt.Errorf("devpts: %v", err)
	}

	if err := os.MkdirAll("/dev/shm", 0755); err != nil {
		return fmt.Errorf("mkdir /dev/shm: %v", err)
	}

	if err := syscall.Mount("tmpfs", "/dev/shm", "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("tmpfs on /dev/shm: %v", err)
	}

	if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, ""); err != nil {
		log.Printf("tmpfs on /run: %v", err)
	}

	// /proc is useful for exposing process details and for
	// interactive debugging sessions.
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /proc was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("proc: %v", err)
		}
	}

	// /sys is useful for retrieving additional status from the
	// kernel, e.g. ethernet device carrier status.
	if err := syscall.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
		if sce, ok := err.(syscall.Errno); ok && sce == syscall.EBUSY {
			// /sys was already mounted (common in setups using nfsroot= or initramfs)
		} else {
			return fmt.Errorf("sys: %v", err)
		}
	}

	dev := rootdev.Partition(rootdev.Perm)
	for _, fstype := range []string{"ext4", "vfat"} {
		if err := syscall.Mount(dev, "/perm", fstype, 0, ""); err != nil {
			log.Printf("Could not mount permanent storage partition %s as %s: %v", dev, fstype, err)
		} else {
			break
		}
	}

	if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		log.Printf("cgroup2 on /sys/fs/cgroup: %v", err)
	}

	return nil
}

func mountAdditionalDisks() error {
	data, err := os.ReadFile(filepath.Join("/perm", additionalDisksFile))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// the file does not exist, so we don't need to mount anything
		log.Println("No additional disks to mount")
		return nil
	}
	extra := additionalDisks{}
	if err := json.Unmarshal(data, &extra); err != nil {
		return fmt.Errorf("can not parse %s: %w", additionalDisksFile, err)
	}
	for _, disk := range extra.Disks {
		if err := disk.validate(); err != nil {
			log.Printf("error when validating additonal disk entry: %v", err)
			continue
		}
		if err := disk.mount(); err != nil {
			return fmt.Errorf("can not mount disk with partUUID %s: %w", disk.PartUUID, err)
		}
	}
	return nil
}
