// Package store opens vodarr's database. The downloader and the providers
// share one handle, as bbolt allows only one per file; each keeps its data in
// a bucket of its own.
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

// File is the database in the download folder. It is named after the jobs,
// which it held alone at first.
const File = ".vodarr-jobs.db"

// Version is the database format. Open refuses a newer one, which this
// vodarr might misread.
const Version = 1

var (
	metaBucket = []byte("meta")
	versionKey = []byte("version")
)

// Open opens the database in dir, creating it if needed.
func Open(dir string) (*bolt.DB, error) {
	path := filepath.Join(dir, File)
	// Without a timeout, Open waits forever for another process's lock.
	db, err := bolt.Open(path, 0o666, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, fmt.Errorf("%s is in use by another vodarr", path)
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

// checkVersion records Version in a database from before versioning.
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
		return fmt.Errorf("format version %d is from a newer vodarr, which this one (%d) can't read", n, Version)
	}
	return nil
}
