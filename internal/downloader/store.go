package downloader

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	bolt "go.etcd.io/bbolt"
)

var jobsBucket = []byte("jobs")

// loadJobs returns the jobs saved in db, oldest first.
func loadJobs(db *bolt.DB) ([]*Job, error) {
	var jobs []*Job
	err := db.Update(func(tx *bolt.Tx) error {
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
		return nil, fmt.Errorf("loading jobs from %s: %w", db.Path(), err)
	}
	// Keys are random IDs, so restore the order jobs were added in.
	slices.SortFunc(jobs, func(a, b *Job) int {
		return cmp.Or(a.Added.Compare(b.Added), strings.Compare(a.ID, b.ID))
	})
	return jobs, nil
}

// put saves job. q.mu must be held, so saves of one job can't reorder.
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

// remove deletes jobs from the database. q.mu must be held.
func (q *Queue) remove(ids ...string) error {
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
		return fmt.Errorf("removing jobs %v: %w", ids, err)
	}
	return nil
}
