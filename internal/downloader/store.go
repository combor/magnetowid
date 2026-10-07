package downloader

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var (
	jobsBucket     = []byte("jobs")
	archivedBucket = []byte("archived_jobs")
	queueBucket    = []byte("queue")
	pausedKey      = []byte("paused")
)

func loadJobs(db *bolt.DB, bucket []byte) ([]*Job, error) {
	var jobs []*Job
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
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
		return nil, fmt.Errorf("loading %s from %s: %w", bucket, db.Path(), err)
	}
	// Random job IDs do not preserve insertion order.
	slices.SortFunc(jobs, func(a, b *Job) int {
		return cmp.Or(a.Added.Compare(b.Added), strings.Compare(a.ID, b.ID))
	})
	return jobs, nil
}

func loadPaused(db *bolt.DB) (bool, error) {
	var paused bool
	err := db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(queueBucket); b != nil {
			paused = b.Get(pausedKey) != nil
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("loading the queue's state from %s: %w", db.Path(), err)
	}
	return paused, nil
}

// Requires q.mu.
func (q *Queue) putPaused(paused bool) error {
	err := q.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(queueBucket)
		if err != nil {
			return err
		}
		if paused {
			return b.Put(pausedKey, []byte("1"))
		}
		return b.Delete(pausedKey)
	})
	if err != nil {
		return fmt.Errorf("saving the queue's pause: %w", err)
	}
	return nil
}

// Requires q.mu to preserve save order.
func (q *Queue) put(job *Job) error {
	v, err := json.Marshal(job)
	if err == nil {
		err = q.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(jobsBucket).Put([]byte(job.ID), v)
		})
	}
	if err != nil {
		return fmt.Errorf("saving job %s: %w", job.ID, err)
	}
	return nil
}

// archive moves a finished job out of client history atomically. Requires q.mu.
func (q *Queue) archive(job *Job) error {
	v, err := json.Marshal(job)
	if err == nil {
		err = q.db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket(archivedBucket).Put([]byte(job.ID), v); err != nil {
				return err
			}
			return tx.Bucket(jobsBucket).Delete([]byte(job.ID))
		})
	}
	if err != nil {
		return fmt.Errorf("archiving job %s: %w", job.ID, err)
	}
	return nil
}

// remove permanently forgets jobs in either bucket. Requires q.mu.
func (q *Queue) remove(ids ...string) error {
	err := q.db.Update(func(tx *bolt.Tx) error {
		for _, bucket := range [][]byte{jobsBucket, archivedBucket} {
			b := tx.Bucket(bucket)
			for _, id := range ids {
				if err := b.Delete([]byte(id)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("removing jobs %v: %w", ids, err)
	}
	return nil
}
