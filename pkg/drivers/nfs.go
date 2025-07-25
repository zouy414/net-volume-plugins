package drivers

import (
	"context"
	"docker-volume-plugin/pkg/drivers/apis"
	"docker-volume-plugin/pkg/drivers/store/badger"
	"docker-volume-plugin/pkg/log"
	"docker-volume-plugin/pkg/utils"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"sync"
	"time"
)

func init() {
	registerFactory("nfs", nfsFactory)
}

func nfsFactory(ctx context.Context, logger *log.Logger, propagatedMountpoint string, driverOptions string) (apis.Driver, error) {
	opts := &nfsOptions{
		PurgeAfterDelete: false,
		MountOptions:     []string{"nfsvers=4", "rw", "noatime", "rsize=8192", "wsize=8192", "tcp", "timeo=14", "sync"},
	}
	err := json.Unmarshal([]byte(driverOptions), opts)
	if err != nil {
		return nil, fmt.Errorf("failed to parse driver options: %v", err)
	}

	// Mount NFS share to a local mount point
	err = os.MkdirAll(propagatedMountpoint, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create NFS mount point directory: %v", err)
	}

	if opts.Address != "nfs-server.mock" {
		err = utils.MountNFS(opts.Address, opts.RemotePath, propagatedMountpoint, opts.MountOptions)
		if err != nil {
			return nil, fmt.Errorf("failed to mount NFS share: %v", err)
		}
	}

	return &nfs{
		logger: logger,
		opts:   opts,
		db: badger.NewBadgerDB(
			logger.WithService("badger").WithLogLevel(log.WarnLevel),
			path.Join(propagatedMountpoint, "metadata.db"),
			path.Join(propagatedMountpoint, "metadata.db.lock"),
		),
		rootPath:     propagatedMountpoint,
		lock:         &sync.RWMutex{},
		reservedPath: []string{"metadata.db", "metadata.db.lock"},
	}, nil
}

type nfsOptions struct {
	// Address of NFS server
	Address string `json:"address"`
	// RemotePath of NFS exported
	RemotePath string `json:"remotePath"`
	// MountOptions for NFS
	MountOptions []string `json:"mountOptions,omitempty"`
	// PurgeAfterDelete indicates whether to purge the volume data after deletion
	PurgeAfterDelete bool `json:"purgeAfterDelete,omitempty"`
}

type nfs struct {
	logger       *log.Logger
	opts         *nfsOptions
	db           *badger.DB
	rootPath     string
	lock         *sync.RWMutex
	reservedPath []string
}

func (n *nfs) Create(name string, options map[string]string) (err error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if slices.Contains(n.reservedPath, name) {
		return fmt.Errorf("volume name %s is reserved, please choose a different name", name)
	}

	purgeAfterDelete := n.opts.PurgeAfterDelete
	for key, value := range options {
		switch key {
		case "purgeAfterDelete":
			purgeAfterDelete, err = strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("invalid value for purgeAfterDelete: %v", err)
			}
		default:
			return fmt.Errorf("unknown option %s with value %s, ignoring", key, value)
		}
	}

	n.logger.Infof("create volume %s", name)

	return n.db.CreateVolumeMetadata(name, func(volumeMetadata *apis.VolumeMetadata) error {
		*volumeMetadata = apis.VolumeMetadata{
			Mountpoint: path.Join(name, "_data"),
			CreatedAt:  time.Now(),
			Spec: &apis.VolumeSpec{
				PurgeAfterDelete: purgeAfterDelete,
			},
			Status: &apis.VolumeStatus{
				MountBy: "",
			},
		}

		return os.MkdirAll(path.Join(n.rootPath, volumeMetadata.Mountpoint), 0755)
	},
	)
}

func (n *nfs) List() (map[string]*apis.VolumeMetadata, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.logger.Info("list volumes")

	return n.db.GetVolumeMetadataMap()
}

func (n *nfs) Get(name string) (*apis.VolumeMetadata, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.logger.Infof("get volume %s", name)

	return n.db.GetVolumeMetadata(name)
}

func (n *nfs) Remove(name string) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.logger.Infof("remove volume %s", name)
	return n.db.DeleteVolumeMetadata(name, func(volumeMetadata *apis.VolumeMetadata) error {
		if len(volumeMetadata.Status.MountBy) != 0 {
			return fmt.Errorf("volume %s is mounted by %s, unmount it before removing", name, volumeMetadata.Status.MountBy)
		}

		if volumeMetadata.Spec.PurgeAfterDelete {
			err := os.RemoveAll(path.Join(n.rootPath, name))
			if err != nil {
				return fmt.Errorf("failed to remove volume data: %v", err)
			}
		}
		return nil
	})
}

func (n *nfs) Path(name string) (string, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.logger.Infof("path volume %s", name)

	volumeMetadata, err := n.db.GetVolumeMetadata(name)

	return volumeMetadata.Mountpoint, err
}

func (n *nfs) Mount(name string, id string) (string, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.logger.Infof("mount volume %s for %s", name, id)
	return path.Join(name, "_data"), n.db.SetVolumeMetadata(name, func(volumeMetadata *apis.VolumeMetadata) error {
		if len(volumeMetadata.Status.MountBy) != 0 {
			return fmt.Errorf("volume %s is already mounted", name)
		}

		volumeMetadata.Status.MountBy = id
		return nil
	})
}

func (n *nfs) Unmount(name string, id string) error {
	n.lock.Lock()
	defer n.lock.Unlock()

	n.logger.Infof("unmount volume %s from %s", name, id)

	return n.db.SetVolumeMetadata(name, func(volumeMetadata *apis.VolumeMetadata) error {
		if len(volumeMetadata.Status.MountBy) == 0 {
			return fmt.Errorf("volume %s is not mounted", name)
		}

		if volumeMetadata.Status.MountBy != id {
			return fmt.Errorf("volume %s already mounted by %s", name, volumeMetadata.Status.MountBy)
		}

		volumeMetadata.Status.MountBy = ""
		return nil
	})
}

func (n *nfs) Destroy() error {
	err := n.db.Close()
	if err != nil {
		n.logger.Warningf("failed to close badger db: %v", err)
	}

	if n.opts.Address != "nfs-server.mock" {
		err = utils.Umount(n.rootPath)
		if err != nil {
			return fmt.Errorf("failed to unmount NFS mount root path %s: %v", n.rootPath, err)
		}
	}

	return nil
}
