package downloader

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// dbFile is the job database in the download folder. It keeps the queue and
// history across restarts.
const dbFile = ".vodarr-jobs.db"

var jobsBucket = []byte("jobs")

// openDB opens the job database in dir and returns its jobs, oldest first.
func openDB(dir string) (*bolt.DB, []*Job, error) {
	path := filepath.Join(dir, dbFile)
	// Without a timeout, Open waits forever for another process's lock.
	db, err := bolt.Open(path, 0o666, &bolt.Options{Timeout: time.Second})
	if errors.Is(err, bolterrors.ErrTimeout) {
		return nil, nil, fmt.Errorf("%s is in use by another vodarr", path)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	var jobs []*Job
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(jobsBucket)
		if err != nil {
			return err
		}
		return b.ForEach(func(k, v []byte) error {
			var j Job
			if err := json.Unmarshal(v, &j); err != nil {
				return fmt.Errorf("job %s: %w", k, err)
			}
			jobs = append(jobs, &j)
			return nil
		})
	})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("loading %s: %w", path, err)
	}
	// Keys are random IDs, so restore the order jobs were added in.
	slices.SortFunc(jobs, func(a, b *Job) int {
		return cmp.Or(a.Added.Compare(b.Added), strings.Compare(a.ID, b.ID))
	})
	return db, jobs, nil
}

// put saves job. q.mu must be held, so saves of one job can't reorder.
func (q *Queue) put(job *Job) {
	v, err := json.Marshal(job)
	if err == nil {
		err = q.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(jobsBucket).Put([]byte(job.ID), v)
		})
	}
	if err != nil {
		q.log.Warn("saving job", "id", job.ID, "err", err)
	}
}

// remove deletes jobs from the database. q.mu must be held.
func (q *Queue) remove(ids ...string) {
	err := q.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket)
		for _, id := range ids {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		q.log.Warn("removing jobs", "ids", ids, "err", err)
	}
}

// Close closes the job database. Call it after Run has returned.
func (q *Queue) Close() error {
	return q.db.Close()
}
