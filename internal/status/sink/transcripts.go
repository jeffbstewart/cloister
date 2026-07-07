// Copyright 2026 Jeffrey B. Stewart
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sink

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jeffbstewart/cloister/internal/runid"
)

// postTranscript stores a research call's URLs-only transcript, gzip'd at
// research/<shard>/<opId>.gz (sharded on the opId's random tail), size-capped
// as a backstop (the scholar caps and flags before upload).  After each store it
// prunes the store to the retention cap, oldest first — the transcript store is
// never an unbounded cache.
func (s *Server) postTranscript(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.researchDir, id.Shard())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "mkdir", http.StatusInternalServerError)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, id.String()+".gz"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, "open transcript", http.StatusInternalServerError)
		return
	}
	gz := gzip.NewWriter(f)
	_, copyErr := io.Copy(gz, io.LimitReader(r.Body, s.cfg.MaxTranscriptBytes))
	gzErr := gz.Close()
	closeErr := f.Close()
	if copyErr != nil || gzErr != nil || closeErr != nil {
		http.Error(w, "store transcript", http.StatusInternalServerError)
		return
	}
	s.pruneTranscripts()
	w.WriteHeader(http.StatusNoContent)
}

// researchPage serves a stored transcript decompressed for the operator's
// browser. text/plain + nosniff — the transcript is agent-influenced text and
// must never render as HTML.
func (s *Server) researchPage(w http.ResponseWriter, r *http.Request) {
	id, err := runid.Parse(r.PathValue("opId"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(filepath.Join(s.researchDir, id.Shard(), id.String()+".gz"))
	if err != nil {
		http.Error(w, "no transcript for op "+id.String(), http.StatusNotFound)
		return
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		http.Error(w, "corrupt transcript", http.StatusInternalServerError)
		return
	}
	defer gz.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, gz)
}

// pruneTranscripts enforces the total-size retention cap, deleting oldest first.
// Best-effort: a walk or remove error just leaves that file in place.
func (s *Server) pruneTranscripts() {
	if s.cfg.TranscriptRetention <= 0 {
		return
	}
	type entry struct {
		path string
		name string // "<opId>.gz" — opId is a runid UUIDv7, so lexical order is creation order
		size int64
	}
	var files []entry
	var total int64
	_ = filepath.WalkDir(s.researchDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".gz") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, entry{path, d.Name(), info.Size()})
		total += info.Size()
		return nil
	})
	if total <= s.cfg.TranscriptRetention {
		return
	}
	// Oldest first by opId, not filesystem mtime: runid mints UUIDv7s with a
	// same-millisecond monotonic counter, so their lexical order is creation
	// order even for a burst of stores — mtime resolution guarantees no such
	// thing (files written in the same tick would be indistinguishable).
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, f := range files {
		if total <= s.cfg.TranscriptRetention {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
