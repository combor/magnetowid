// Package store shares one bbolt handle across the downloader and providers.
// Each owns a bucket; bbolt allows only one handle per file.
package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

const File = ".magnetowid-jobs.db"

// Open rejects database versions newer than Version.
const Version = 1

var (
	metaBucket = []byte("meta")
	versionKey = []byte("version")
)

func Open(dir string) (*bolt.DB, error) {
	path := filepath.Join(dir, File)
	// Bound the wait for another process's file lock.
	db, err := bolt.Open(path, 0o666, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("%s is in use by another magnetowid", path)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if err := db.Update(checkVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return db, nil
}

// Initialize databases created before format versioning.
func checkVersion(tx *bolt.Tx) error {
	b, err := tx.CreateBucketIfNotExists(metaBucket)
	if err != nil {
		return err
	}
	v := b.Get(versionKey)
	if v == nil {
		return b.Put(versionKey, []byte(strconv.Itoa(Version)))
	}
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return fmt.Errorf("unknown format version %q", v)
	}
	if n > Version {
		return fmt.Errorf("format version %d is from a newer magnetowid, which this one (%d) can't read", n, Version)
	}
	return nil
}
