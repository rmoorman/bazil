package db

import (
	"crypto/rand"
	"errors"

	"bazil.org/bazil/tokens"
	"github.com/boltdb/bolt"
)

var (
	ErrVolNameInvalid   = errors.New("invalid volume name")
	ErrVolNameNotFound  = errors.New("volume name not found")
	ErrVolNameExist     = errors.New("volume name exists already")
	ErrVolumeIDNotFound = errors.New("volume ID not found")
)

var (
	bucketVolume       = []byte(tokens.BucketVolume)
	bucketVolName      = []byte(tokens.BucketVolName)
	volumeStateDir     = []byte(tokens.VolumeStateDir)
	volumeStateInode   = []byte(tokens.VolumeStateInode)
	volumeStateSnap    = []byte(tokens.VolumeStateSnap)
	volumeStateStorage = []byte(tokens.VolumeStateStorage)
)

func (tx *Tx) initVolumes() error {
	if _, err := tx.CreateBucketIfNotExists(bucketVolume); err != nil {
		return err
	}
	if _, err := tx.CreateBucketIfNotExists(bucketVolName); err != nil {
		return err
	}
	return nil
}

func (tx *Tx) Volumes() *Volumes {
	p := &Volumes{
		volumes: tx.Bucket(bucketVolume),
		names:   tx.Bucket(bucketVolName),
	}
	return p
}

type Volumes struct {
	volumes *bolt.Bucket
	names   *bolt.Bucket
}

func (b *Volumes) GetByName(name string) (*Volume, error) {
	volID := b.names.Get([]byte(name))
	if volID == nil {
		return nil, ErrVolNameNotFound
	}
	bv := b.volumes.Bucket(volID)
	v := &Volume{
		b:  bv,
		id: volID,
	}
	return v, nil
}

func (b *Volumes) GetByVolumeID(volID *VolumeID) (*Volume, error) {
	bv := b.volumes.Bucket(volID[:])
	if bv == nil {
		return nil, ErrVolumeIDNotFound
	}
	v := &Volume{
		b:  bv,
		id: append([]byte(nil), volID[:]...),
	}
	return v, nil
}

// Create a totally new volume, not yet shared with any peers.
//
// If the name exists already, returns ErrVolNameExist.
func (b *Volumes) Create(name string, storage string, sharingKey *SharingKey) (*Volume, error) {
	if name == "" {
		return nil, ErrVolNameInvalid
	}
	n := []byte(name)
	if v := b.names.Get(n); v != nil {
		return nil, ErrVolNameExist
	}

random:
	id, err := randomVolumeID()
	if err != nil {
		return nil, err
	}
	bv, err := b.volumes.CreateBucket(id[:])
	if err == bolt.ErrBucketExists {
		goto random
	}
	if err != nil {
		return nil, err
	}

	if err := b.names.Put(n, id[:]); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateDir); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateInode); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateSnap); err != nil {
		return nil, err
	}
	if _, err := bv.CreateBucket(volumeStateStorage); err != nil {
		return nil, err
	}
	v := &Volume{
		b:  bv,
		id: id[:],
	}
	if err := v.Storage().Add("default", storage, sharingKey); err != nil {
		return nil, err
	}
	return v, nil
}

const VolumeIDLen = 64

type VolumeID [VolumeIDLen]byte

func randomVolumeID() (*VolumeID, error) {
	var id VolumeID
	_, err := rand.Read(id[:])
	if err != nil {
		return nil, err
	}
	return &id, nil
}

type Volume struct {
	b  *bolt.Bucket
	id []byte
}

// VolumeID copies the volume ID to out.
//
// out is valid after the transaction.
func (v *Volume) VolumeID(out *VolumeID) {
	copy(out[:], v.id)
}

func (v *Volume) Storage() *VolumeStorage {
	b := v.b.Bucket(volumeStateStorage)
	return &VolumeStorage{b}
}

// DirBucket returns a bolt bucket for storing directory contents in.
func (v *Volume) DirBucket() *bolt.Bucket {
	return v.b.Bucket(volumeStateDir)
}

// InodeBucket returns a bolt bucket for storing inodes in.
func (v *Volume) InodeBucket() *bolt.Bucket {
	return v.b.Bucket(volumeStateInode)
}

// SnapBucket returns a bolt bucket for storing snapshots in.
func (v *Volume) SnapBucket() *bolt.Bucket {
	return v.b.Bucket(volumeStateSnap)
}
