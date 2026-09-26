// Package sabnzbd implements the subset of the SABnzbd API that Sonarr and
// Radarr use, so vodarr can be added to them as a SABnzbd download client.
package sabnzbd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/combor/vodarr/internal/downloader"
	"github.com/combor/vodarr/internal/nzb"
)

// version is what Sonarr/Radarr's connection test checks (≥ 0.7).
const version = "4.5.1"

// bytesPerSecond estimates job size before ffmpeg reports any (4 Mbit/s).
const bytesPerSecond = 4_000_000 / 8

// maxBodyBytes caps request bodies, i.e. NZB uploads.
const maxBodyBytes = 4 << 20

// Handler serves the SABnzbd API. Mount it at "/api".
type Handler struct {
	Queue      *downloader.Queue
	APIKey     string
	Categories []string
	Log        *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Sonarr/Radarr put apikey in the query string, so check it there before
	// reading any body. FormValue parses multipart bodies (addfile) and merges
	// them with the query; the cap keeps a big upload from filling memory or
	// disk (vodarr's NZBs are tiny).
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	key := r.URL.Query().Get("apikey")
	if key == "" {
		key = r.FormValue("apikey")
	}
	if key != h.APIKey {
		writeJSON(w, errorResponse("API Key Incorrect"))
		return
	}
	switch mode := r.FormValue("mode"); mode {
	case "version":
		writeJSON(w, map[string]string{"version": version})
	case "get_config":
		writeJSON(w, map[string]any{"config": h.config()})
	case "fullstatus":
		writeJSON(w, map[string]any{"status": map[string]string{"completedir": h.Queue.Dir()}})
	case "addfile":
		h.addFile(w, r)
	case "queue":
		if r.FormValue("name") == "delete" {
			h.delete(w, r)
			return
		}
		writeJSON(w, map[string]any{"queue": h.queue(r.FormValue("category"))})
	case "history":
		if r.FormValue("name") == "delete" {
			h.delete(w, r)
			return
		}
		writeJSON(w, map[string]any{"history": h.history(r.FormValue("category"))})
	default:
		writeJSON(w, errorResponse(fmt.Sprintf("mode %q not supported", mode)))
	}
}

func (h *Handler) addFile(w http.ResponseWriter, r *http.Request) {
	file, header, err := formFile(r, "name", "nzbfile")
	if err != nil {
		writeJSON(w, errorResponse("no NZB file: "+err.Error()))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		writeJSON(w, errorResponse(err.Error()))
		return
	}
	ref, err := nzb.Decode(data)
	if err != nil {
		// A real NZB routed here by mistake; refuse it so the *arr tries elsewhere.
		h.Log.Warn("rejected NZB", "file", header.Filename, "err", err)
		writeJSON(w, errorResponse(err.Error()))
		return
	}
	name := r.FormValue("nzbname")
	if name == "" {
		name = strings.TrimSuffix(header.Filename, ".nzb")
	}
	id := h.Queue.Add(name, header.Filename, r.FormValue("cat"), ref)
	writeJSON(w, map[string]any{"status": true, "nzo_ids": []string{id}})
}

func formFile(r *http.Request, fields ...string) (multipart.File, *multipart.FileHeader, error) {
	var err error
	for _, f := range fields {
		var file multipart.File
		var header *multipart.FileHeader
		if file, header, err = r.FormFile(f); err == nil {
			return file, header, nil
		}
	}
	return nil, nil, err
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	deleteFiles := r.FormValue("del_files") == "1"
	var ids []string
	for _, id := range strings.Split(r.FormValue("value"), ",") {
		if id = strings.TrimSpace(id); id != "" && h.Queue.Delete(id, deleteFiles) {
			ids = append(ids, id)
		}
	}
	writeJSON(w, map[string]any{"status": true, "nzo_ids": ids})
}

type category struct {
	Name     string `json:"name"`
	Order    int    `json:"order"`
	PP       string `json:"pp"`
	Script   string `json:"script"`
	Dir      string `json:"dir"`
	Priority int    `json:"priority"`
}

// config is what the connection test inspects: categories must exist with a
// plain dir, and sorting must be off.
func (h *Handler) config() map[string]any {
	cats := []category{{Name: "*", PP: "3", Script: "None", Dir: ""}}
	for i, c := range h.Categories {
		cats = append(cats, category{Name: c, Order: i + 1, PP: "3", Script: "None", Dir: c})
	}
	return map[string]any{
		"misc": map[string]any{
			"complete_dir":             h.Queue.Dir(),
			"tv_categories":            []string{},
			"enable_tv_sorting":        false,
			"movie_categories":         []string{},
			"enable_movie_sorting":     false,
			"date_categories":          []string{},
			"enable_date_sorting":      false,
			"pre_check":                false,
			"history_retention":        "",
			"history_retention_option": "all",
			"history_retention_number": 0,
		},
		"categories": cats,
		"servers":    []any{},
		"sorters":    []any{},
	}
}

type queueSlot struct {
	ID         string `json:"nzo_id"`
	Index      int    `json:"index"`
	Filename   string `json:"filename"`
	Category   string `json:"cat"`
	Status     string `json:"status"`
	Priority   string `json:"priority"`
	MB         string `json:"mb"`
	MBLeft     string `json:"mbleft"`
	Percentage string `json:"percentage"`
	Timeleft   string `json:"timeleft"`
}

func (h *Handler) queue(cat string) map[string]any {
	slots := []queueSlot{}
	for _, j := range h.Queue.Jobs() {
		if (j.Status != downloader.StatusQueued && j.Status != downloader.StatusDownloading) ||
			(cat != "" && j.Category != cat) {
			continue
		}
		total := estimatedSize(j)
		slots = append(slots, queueSlot{
			ID:         j.ID,
			Index:      len(slots),
			Filename:   j.Name,
			Category:   j.Category,
			Status:     string(j.Status),
			Priority:   "Normal",
			MB:         megabytes(total),
			MBLeft:     megabytes(int64(float64(total) * (1 - j.Fraction))),
			Percentage: strconv.Itoa(int(j.Fraction * 100)),
			Timeleft:   timeLeft(j),
		})
	}
	return map[string]any{"paused": false, "slots": slots}
}

type historySlot struct {
	ID           string `json:"nzo_id"`
	Name         string `json:"name"`
	NZBName      string `json:"nzb_name"`
	Category     string `json:"category"`
	Status       string `json:"status"`
	Storage      string `json:"storage"`
	Bytes        int64  `json:"bytes"`
	DownloadTime int    `json:"download_time"`
	FailMessage  string `json:"fail_message"`
}

// history lists finished jobs, newest first like SABnzbd.
func (h *Handler) history(cat string) map[string]any {
	jobs := h.Queue.Jobs()
	slots := []historySlot{}
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		if (j.Status != downloader.StatusCompleted && j.Status != downloader.StatusFailed) ||
			(cat != "" && j.Category != cat) {
			continue
		}
		slots = append(slots, historySlot{
			ID:           j.ID,
			Name:         j.Name,
			NZBName:      j.NZBName,
			Category:     j.Category,
			Status:       string(j.Status),
			Storage:      j.Storage,
			Bytes:        j.Bytes,
			DownloadTime: int(j.Finished.Sub(j.Started).Seconds()),
			FailMessage:  j.Error,
		})
	}
	return map[string]any{"noofslots": len(slots), "slots": slots}
}

// estimatedSize extrapolates from bytes written so far, or guesses from the
// duration before ffmpeg has reported anything.
func estimatedSize(j downloader.Job) int64 {
	if j.Fraction > 0.01 && j.Bytes > 0 {
		return int64(float64(j.Bytes) / j.Fraction)
	}
	return int64(j.Ref.Duration) * bytesPerSecond
}

func timeLeft(j downloader.Job) string {
	var left time.Duration
	if j.Status == downloader.StatusDownloading && j.Fraction > 0.01 {
		elapsed := time.Since(j.Started)
		left = time.Duration(float64(elapsed) * (1 - j.Fraction) / j.Fraction)
	}
	s := int(left.Seconds())
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

func megabytes(b int64) string {
	return strconv.FormatFloat(float64(b)/(1<<20), 'f', 2, 64)
}

func errorResponse(msg string) map[string]any {
	return map[string]any{"status": false, "error": msg}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
