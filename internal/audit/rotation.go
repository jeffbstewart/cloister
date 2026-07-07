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

package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Rotation defaults.
const (
	DefaultMaxBytes    = 10 << 20 // rotate the current file past 10 MB
	DefaultGenerations = 5        // audit.jsonl.1 … .5 are kept
)

// Options tunes rotation; the zero value selects the defaults.
type Options struct {
	MaxBytes    int64 // rotate when the current file reaches this; 0 → DefaultMaxBytes
	Generations int   // rotated generations kept; 0 → DefaultGenerations
}

// ApplyDefaults replaces each unset (zero or negative) field with its package
// default.
func (o *Options) ApplyDefaults() {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Generations <= 0 {
		o.Generations = DefaultGenerations
	}
}

// Log is an append-only, self-rotating JSONL writer, safe for concurrent use.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	max  int64
	gens int
}

// Open opens (or creates) the audit file in append-only mode.
func Open(path string, opts Options) (*Log, error) {
	opts.ApplyDefaults()
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Log{path: path, f: f, size: fi.Size(), max: opts.MaxBytes, gens: opts.Generations}, nil
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// Append writes one record as a single JSON line, rejecting a record whose
// required Header is incomplete, then rotates if the file has reached the size
// threshold — a record is never split across generations.  Append stamps Time
// unconditionally: the writer's clock is authoritative, so a caller can
// neither backdate nor forward-date the trail.
func (l *Log) Append(r Record) error {
	r.Time = time.Now().UTC()
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	n, werr := l.f.Write(b)
	l.size += int64(n)
	if werr != nil {
		return werr
	}
	if l.size >= l.max {
		return l.rotate()
	}
	return nil
}

// rotate shifts audit.jsonl → .1 → … → .N (dropping the oldest) and starts
// a fresh current file.  Callers hold l.mu.
func (l *Log) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, l.gens))
	for i := l.gens - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, fmt.Sprintf("%s.%d", l.path, i+1))
		}
	}
	renameErr := os.Rename(l.path, l.path+".1")
	f, err := openAppend(l.path)
	if err != nil {
		return errors.Join(renameErr, err)
	}
	l.f = f
	if renameErr != nil {
		// The old file could not be moved (e.g. held open elsewhere on
		// Windows): keep appending to it rather than losing records.
		fi, statErr := f.Stat()
		if statErr == nil {
			l.size = fi.Size()
		}
		return renameErr
	}
	l.size = 0
	return nil
}

// Close closes the underlying file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
