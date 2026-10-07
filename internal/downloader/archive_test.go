package downloader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/magnetowid/internal/nzb"
	"github.com/combor/magnetowid/internal/provider"
)

func TestClientRemovalArchivesFinishedJobs(t *testing.T) {
	for _, failed := range []bool{false, true} {
		for _, deleteFiles := range []bool{false, true} {
			t.Run(fmt.Sprintf("failed=%t/files=%t", failed, deleteFiles), func(t *testing.T) {
				dir := t.TempDir()
				p := &fakeProvider{}
				if failed {
					p.err = provider.ErrUnavailable
				}
				q := newQueue(t, dir, p, &fakeEngine{})
				stop := run(t, q)
				want := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, nzb.Ref{Provider: "fake", ID: "1"}))
				stop()
				if ok, err := q.RemoveForClient(want.ID, deleteFiles); !ok || err != nil {
					t.Fatalf("remove = %v, %v", ok, err)
				}
				check := func() {
					t.Helper()
					if len(q.Jobs()) != 0 {
						t.Fatalf("client still sees archived jobs: %+v", q.Jobs())
					}
					all := q.AllJobs()
					if len(all) != 1 || !all[0].Archived {
						t.Fatalf("UI history = %+v", all)
					}
					gotJSON, _ := json.Marshal(all[0])
					wantJSON, _ := json.Marshal(want)
					if string(gotJSON) != string(wantJSON) {
						t.Errorf("archive = %s, want %s", gotJSON, wantJSON)
					}
				}
				check()
				q.db.Close()
				q = newQueue(t, dir, p, &fakeEngine{})
				check()
				if !failed {
					_, err := os.Stat(want.Storage)
					if (deleteFiles && !os.IsNotExist(err)) || (!deleteFiles && err != nil) {
						t.Errorf("files after removal: %v", err)
					}
				}
				for _, id := range []string{want.ID, "unknown"} {
					if ok, err := q.RemoveForClient(id, true); ok || err != nil {
						t.Errorf("repeated/unknown removal = %v, %v", ok, err)
					}
				}
				check()
				if ok, err := q.Cancel(want.ID); ok || err != nil {
					t.Errorf("cancel archive = %v, %v", ok, err)
				}
				if ok, err := q.Delete(want.ID, true); !ok || err != nil {
					t.Fatalf("forget archive = %v, %v", ok, err)
				}
				if !failed && !deleteFiles {
					if _, err := os.Stat(want.Storage); err != nil {
						t.Errorf("forgetting archive deleted retained files: %v", err)
					}
				}
				q.db.Close()
				q = newQueue(t, dir, p, &fakeEngine{})
				if len(q.AllJobs()) != 0 {
					t.Fatal("forgotten archive returned after restart")
				}
			})
		}
	}
}

func TestArchivedStorageCanBeReusedSafely(t *testing.T) {
	q := startQueue(t, &fakeProvider{}, &fakeEngine{})
	ref := nzb.Ref{Provider: "fake", ID: "1"}
	first := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, ref))
	if ok, err := q.RemoveForClient(first.ID, true); !ok || err != nil {
		t.Fatalf("archive = %v, %v", ok, err)
	}
	second := waitFinished(t, q, add(t, q, "Movie.2020", "movies", 0, ref))
	if second.Storage != first.Storage {
		t.Fatalf("archive reserved an unused folder: %q != %q", second.Storage, first.Storage)
	}
	if ok, err := q.RemoveForClient(first.ID, true); ok || err != nil {
		t.Fatalf("repeat removal = %v, %v", ok, err)
	}
	if ok, err := q.Delete(first.ID, true); !ok || err != nil {
		t.Fatalf("forget archive = %v, %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(second.Storage, "Movie.2020.mp4")); err != nil {
		t.Errorf("new download lost its files: %v", err)
	}
}

func TestClientRemovalCancelsUnfinishedJobs(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			e := &blockingEngine{started: make(chan string, 1)}
			q := newQueue(t, t.TempDir(), &fakeProvider{}, e)
			id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"})
			stop := func() {}
			if running {
				stop = run(t, q)
				<-e.started
			}
			if ok, err := q.RemoveForClient(id, true); !ok || err != nil {
				t.Fatalf("remove = %v, %v", ok, err)
			}
			stop()
			if len(q.AllJobs()) != 0 {
				t.Fatal("cancelled job was archived")
			}
			if _, err := os.Stat(q.workDir(id)); !os.IsNotExist(err) {
				t.Errorf("unfinished files remain: %v", err)
			}
		})
	}
}

func TestArchiveWriteFailuresLeaveRecordsAndFiles(t *testing.T) {
	q := newQueue(t, t.TempDir(), &fakeProvider{}, &fakeEngine{})
	stop := run(t, q)
	ref := nzb.Ref{Provider: "fake", ID: "1"}
	a := waitFinished(t, q, add(t, q, "archived", "tv", 0, ref))
	b := waitFinished(t, q, add(t, q, "completed", "tv", 0, ref))
	stop()
	if _, err := q.RemoveForClient(a.ID, false); err != nil {
		t.Fatal(err)
	}
	before := q.AllJobs()
	q.db.Close()
	if ok, err := q.RemoveForClient(b.ID, true); ok || err == nil {
		t.Errorf("archive without saving = %v, %v", ok, err)
	}
	if ok, err := q.Delete(a.ID, true); ok || err == nil {
		t.Errorf("delete archive without saving = %v, %v", ok, err)
	}
	q.mu.Lock()
	q.prune(time.Now().Add(HistoryRetention + time.Hour))
	q.mu.Unlock()
	if !reflect.DeepEqual(before, q.AllJobs()) {
		t.Fatal("failed writes changed history")
	}
	for _, j := range []Job{a, b} {
		if _, err := os.Stat(j.Storage); err != nil {
			t.Errorf("failed write removed files: %v", err)
		}
	}
}

func TestPruneArchivesAndHistory(t *testing.T) {
	dir := t.TempDir()
	q := newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	// Keep the exact boundary stable when reopening the database below.
	now := time.Now().Add(time.Hour)
	const retention = 90 * 24 * time.Hour
	var want, folders []string
	for _, archived := range []bool{false, true} {
		for _, status := range []Status{StatusCompleted, StatusFailed} {
			for _, age := range []time.Duration{retention + time.Nanosecond, retention, 45 * 24 * time.Hour} {
				id := fmt.Sprintf("%t-%s-%s", archived, status, age)
				j := &Job{ID: id, Status: status, Finished: now.Add(-age), Storage: filepath.Join(dir, id)}
				if err := os.Mkdir(j.Storage, 0o755); err != nil {
					t.Fatal(err)
				}
				folders = append(folders, j.Storage)
				if err := q.put(j); err != nil {
					t.Fatal(err)
				}
				q.jobs[id], q.order = j, append(q.order, id)
				if archived {
					if _, err := q.RemoveForClient(id, false); err != nil {
						t.Fatal(err)
					}
				}
				if age <= retention {
					want = append(want, id)
				}
			}
		}
	}
	// A retrying job has a finish time but must never be pruned.
	id := add(t, q, "retrying", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"})
	q.jobs[id].Finished = now.Add(-2 * HistoryRetention)
	if err := q.put(q.jobs[id]); err != nil {
		t.Fatal(err)
	}
	want = append(want, id)
	q.mu.Lock()
	q.prune(now)
	q.mu.Unlock()
	q.db.Close()
	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	var got []string
	for _, j := range q.AllJobs() {
		got = append(got, j.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("retained %v, want %v", got, want)
	}
	for _, folder := range folders {
		if _, err := os.Stat(folder); err != nil {
			t.Errorf("pruning removed a download folder: %v", err)
		}
	}
}

func TestExistingDatabaseGainsArchive(t *testing.T) {
	dir := t.TempDir()
	q := newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	id := add(t, q, "a", "tv", 0, nzb.Ref{Provider: "fake", ID: "1"})
	// Reproduce the old layout, with no archive bucket.
	if err := q.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(archivedBucket) }); err != nil {
		t.Fatal(err)
	}
	q.db.Close()
	q = newQueue(t, dir, &fakeProvider{}, &fakeEngine{})
	if jobs := q.AllJobs(); len(jobs) != 1 || jobs[0].ID != id || jobs[0].Archived {
		t.Fatalf("existing jobs changed: %+v", jobs)
	}
}
