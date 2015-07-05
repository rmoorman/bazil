package fs

import (
	"fmt"
	"log"
	"path"
	"strings"
	"sync"

	"bazil.org/bazil/cas/chunks"
	"bazil.org/bazil/db"
	"bazil.org/bazil/fs/clock"
	"bazil.org/bazil/fs/inodes"
	wiresnap "bazil.org/bazil/fs/snap/wire"
	"bazil.org/bazil/fs/wire"
	"bazil.org/bazil/peer"
	wirepeer "bazil.org/bazil/peer/wire"
	"bazil.org/bazil/tokens"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

type Volume struct {
	db         *db.DB
	volID      db.VolumeID
	pubKey     peer.PublicKey
	chunkStore chunks.Store
	root       *dir

	epoch struct {
		mu sync.Mutex
		// Epoch is a logical clock keeping track of file mutations. It
		// increments for every outgoing (dirty) sync of this volume.
		ticks clock.Epoch
		// Have changes been made since epoch ticked?
		dirty bool
	}
}

var _ = fs.FS(&Volume{})
var _ = fs.FSInodeGenerator(&Volume{})

var bucketVolume = []byte(tokens.BucketVolume)
var bucketVolName = []byte(tokens.BucketVolName)
var bucketDir = []byte("dir")
var bucketInode = []byte("inode")
var bucketSnap = []byte("snap")
var bucketStorage = []byte("storage")

func (v *Volume) bucket(tx *db.Tx) *db.Volume {
	vv, err := tx.Volumes().GetByVolumeID(&v.volID)
	if err != nil {
		log.Printf("volume has disappeared: %v: %v", &v.volID, err)
	}
	return vv
}

// Open returns a FUSE filesystem instance serving content from the
// given volume. The result can be passed to bazil.org/fuse/fs#Serve
// to start serving file access requests from the kernel.
//
// Caller guarantees volume ID exists at least as long as requests are
// served for this file system.
func Open(db *db.DB, chunkStore chunks.Store, volumeID *db.VolumeID, pubKey *peer.PublicKey) (*Volume, error) {
	fs := &Volume{}
	fs.db = db
	fs.volID = *volumeID
	fs.pubKey = *pubKey
	fs.chunkStore = chunkStore
	fs.root = newDir(fs, tokens.InodeRoot, nil, "")
	// assume we crashed, to be safe
	fs.epoch.dirty = true
	if err := fs.db.View(fs.initFromDB); err != nil {
		return nil, err
	}
	return fs, nil
}

func (v *Volume) initFromDB(tx *db.Tx) error {
	epoch, err := v.bucket(tx).Epoch()
	if err != nil {
		return err
	}
	v.epoch.ticks = epoch
	return nil
}

func (v *Volume) Root() (fs.Node, error) {
	return v.root, nil
}

func (*Volume) GenerateInode(parent uint64, name string) uint64 {
	return inodes.Dynamic(parent, name)
}

var _ = fs.FSInodeGenerator(&Volume{})

// Snapshot records a snapshot of the volume. The Snapshot message
// itself has not been persisted yet.
func (v *Volume) Snapshot(ctx context.Context, tx *db.Tx) (*wiresnap.Snapshot, error) {
	snapshot := &wiresnap.Snapshot{}
	sde, err := v.root.snapshot(ctx, tx)
	if err != nil {
		return nil, err
	}
	snapshot.Contents = sde
	return snapshot, nil
}

// caller is responsible for locking
//
// TODO nextEpoch only needs to tick if the volume is seeing mutation;
// unmounted is safe?
func (v *Volume) nextEpoch(vb *db.Volume) error {
	if !v.epoch.dirty {
		return nil
	}
	n, err := vb.NextEpoch()
	if err != nil {
		return err
	}
	v.epoch.ticks = n
	v.epoch.dirty = false
	return nil
}

func (v *Volume) dirtyEpoch() clock.Epoch {
	v.epoch.mu.Lock()
	defer v.epoch.mu.Unlock()
	v.epoch.dirty = true
	return v.epoch.ticks
}

// caller must hold v.epoch.mu
func (v *Volume) cleanEpoch() (clock.Epoch, error) {
	if !v.epoch.dirty {
		return v.epoch.ticks, nil
	}
	inc := func(tx *db.Tx) error {
		vb := v.bucket(tx)
		if err := v.nextEpoch(vb); err != nil {
			return err
		}
		return nil
	}
	if err := v.db.Update(inc); err != nil {
		return 0, err
	}
	return v.epoch.ticks, nil
}

func splitPath(p string) (string, string) {
	idx := strings.IndexByte(p, '/')
	if idx == -1 {
		return p, ""
	}
	return p[:idx], p[idx+1:]
}

func (v *Volume) SyncSend(ctx context.Context, dirPath string, send func(*wirepeer.VolumeSyncPullItem) error) error {
	dirPath = path.Clean("/" + dirPath)[1:]

	// First, start a new epoch so all mutations happen after the
	// clocks that are included in the snapshot.
	//
	// We hold the lock over to prevent using clocks from using the
	// new epoch until we have a snapshot started.
	v.epoch.mu.Lock()
	locked := true
	defer func() {
		if locked {
			v.epoch.mu.Unlock()
		}
	}()
	if _, err := v.cleanEpoch(); err != nil {
		return err
	}
	sync := func(tx *db.Tx) error {
		v.epoch.mu.Unlock()
		locked = false

		// NOT HOLDING THE LOCK, accessing database snapshot ONLY

		bucket := v.bucket(tx)
		dirs := bucket.Dirs()
		clocks := bucket.Clock()

		dirInode := v.root.inode
		var dirDE *wire.Dirent

		for dirPath != "" {
			var seg string
			seg, dirPath = splitPath(dirPath)

			de, err := dirs.Get(dirInode, seg)
			if err != nil {
				return err
			}
			// Might not be a dir anymore but that'll just trigger
			// ENOENT on the next round.
			dirInode = de.Inode
			dirDE = de
		}

		// If it's not the root, make sure it's a directory; List below doesn't.
		if dirDE != nil && dirDE.Dir == nil {
			msg := &wirepeer.VolumeSyncPullItem{
				Error: wirepeer.VolumeSyncPullItem_NOT_A_DIRECTORY,
			}
			if err := send(msg); err != nil {
				return err
			}
			return nil
		}

		msg := &wirepeer.VolumeSyncPullItem{
			Peers: map[uint32][]byte{
				// PeerID 0 always refers to myself.
				0: v.pubKey[:],
			},
		}

		cursor := tx.Peers().Cursor()
		for peer := cursor.First(); peer != nil; peer = cursor.Next() {
			// filter what ids are returned here to include only peers
			// authorized for current volumes; avoids leaking information
			// about all of our peers.
			if !peer.Volumes().IsAllowed(bucket) {
				continue
			}

			// TODO hardcoded knowledge of size of peer.ID
			msg.Peers[uint32(peer.ID())] = peer.Pub()[:]
		}

		c := dirs.List(dirInode)
		const maxBatch = 1000
		for item := c.First(); item != nil; item = c.Next() {
			name := item.Name()

			var tmp wire.Dirent
			if err := item.Unmarshal(&tmp); err != nil {
				return err
			}

			de := &wirepeer.Dirent{
				Name: name,
			}
			switch {
			case tmp.File != nil:
				de.File = &wirepeer.File{
					Manifest: tmp.File.Manifest,
				}
			case tmp.Dir != nil:
				de.Dir = &wirepeer.Dir{}
			default:
				return fmt.Errorf("unknown dirent type: %v", tmp)
			}

			clock, err := clocks.Get(dirInode, name)
			if err != nil {
				return err
			}
			// TODO more complex db api would avoid unmarshal-marshal
			// hoops
			clockBuf, err := clock.MarshalBinary()
			if err != nil {
				return err
			}
			de.Clock = clockBuf

			// TODO executable, xattr, acl
			// TODO mtime

			msg.Children = append(msg.Children, de)

			if len(msg.Children) > maxBatch {
				if err := send(msg); err != nil {
					return err
				}
				msg.Reset()
			}
		}

		if len(msg.Children) > 0 || msg.Peers != nil {
			if err := send(msg); err != nil {
				return err
			}
		}

		return nil
	}
	if err := v.db.View(sync); err != nil {
		return err
	}
	return nil
}

type node interface {
	fs.Node

	marshal() (*wire.Dirent, error)
	setName(name string)
}
