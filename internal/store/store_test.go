package store

import (
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func version(t *testing.T, db *bolt.DB) string {
	t.Helper()
	var v string
	db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(metaBucket); b != nil {
			v = string(b.Get(versionKey))
		}
		return nil
	})
	return v
}

func TestNewDatabaseGetsVersion(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v := version(t, db); v != "1" {
		t.Errorf("version = %q, want 1", v)
	}
}

// A queue saved before versioning opens unchanged.
func TestUnversionedDatabaseOpens(t *testing.T) {
	dir := t.TempDir()
	old, err := bolt.Open(filepath.Join(dir, File), 0o666, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = old.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte("jobs"))
		if err != nil {
			return err
		}
		return b.Put([]byte("SABnzbd_nzo_1"), []byte(`{"id":"SABnzbd_nzo_1"}`))
	})
	old.Close()
	if err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if v := version(t, db); v != "1" {
		t.Errorf("version = %q, want 1", v)
	}
	var job string
	db.View(func(tx *bolt.Tx) error {
		job = string(tx.Bucket([]byte("jobs")).Get([]byte("SABnzbd_nzo_1")))
		return nil
	})
	if job == "" {
		t.Error("job lost")
	}
}

func TestNewerVersionIsRefused(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(metaBucket).Put(versionKey, []byte("2"))
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if db, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer magnetowid") {
		if db != nil {
			db.Close()
		}
		t.Fatalf("err = %v, want a newer version refused", err)
	}
}

// A second magnetowid on the same folder fails instead of waiting for the lock.
func TestDatabaseInUse(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "in use by another magnetowid") {
		t.Fatalf("err = %v", err)
	}
}
