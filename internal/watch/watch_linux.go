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

//go:build linux

package watch

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// mask covers completed content changes and namespace changes.  IN_MODIFY
// is deliberately absent: it fires per write() mid-stream; IN_CLOSE_WRITE
// marks the finished file, and the scribe's atomic rename arrives as
// IN_MOVED_TO — both verified by the 2026-07-07 spike.
const mask = syscall.IN_CREATE | syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_FROM |
	syscall.IN_MOVED_TO | syscall.IN_DELETE | syscall.IN_DELETE_SELF

// Watcher is a recursive inotify watcher over one directory tree.
//
// The inotify descriptor is opened non-blocking and wrapped in an
// os.File so reads park in the runtime poller — a bare blocking
// syscall.Read would NOT be woken by closing the descriptor, deadlocking
// Close (found the hard way: every test hung to the framework timeout).
type Watcher struct {
	root string
	file *os.File // the inotify descriptor, poller-registered

	shouldDescend func(rel string) bool
	onChange      func(rel string)
	onOverflow    func()

	mu   sync.Mutex
	dirs map[int32]string // watch descriptor → root-relative dir ("" = root)
	done chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// New starts watching root recursively.  onChange receives root-relative
// slash paths whose content or existence changed; onOverflow fires when
// the kernel queue overflowed and the caller should full-rescan.
// shouldDescend gates which subdirectories get watches (the librarian
// passes a shield check, keeping watch descriptors off Hidden build
// trees); nil descends everywhere except .git, which is never watched.
//
// Watch-set changes from a reshield apply on the owner's next full
// restart of the watcher; correctness never depends on that, since the
// minute rescan bounds staleness regardless (docs/librarian.md).
func New(root string, shouldDescend func(rel string) bool, onChange func(rel string), onOverflow func()) (*Watcher, error) {
	if onChange == nil {
		return nil, fmt.Errorf("watch: onChange is required")
	}
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("watch: inotify_init: %w", err)
	}
	w := &Watcher{
		root:          root,
		file:          os.NewFile(uintptr(fd), "inotify"),
		shouldDescend: shouldDescend,
		onChange:      onChange,
		onOverflow:    onOverflow,
		dirs:          make(map[int32]string),
		done:          make(chan struct{}),
	}
	if w.file == nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("watch: invalid inotify descriptor")
	}
	if err := w.addTree(""); err != nil {
		w.file.Close()
		return nil, err
	}
	go w.loop()
	return w, nil
}

// Close stops the watcher: os.File.Close wakes the poller-parked read
// loop (a raw close of the fd would not).  Safe to call more than once —
// only the first call touches the descriptor.
func (w *Watcher) Close() error {
	w.closeOnce.Do(func() {
		w.closeErr = w.file.Close()
		<-w.done
	})
	return w.closeErr
}

// addWatch registers one directory without detaching the file from the
// runtime poller (File.Fd would; SyscallConn does not).
func (w *Watcher) addWatch(p string) (int, error) {
	rc, err := w.file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var wd int
	var werr error
	if cerr := rc.Control(func(fd uintptr) {
		wd, werr = syscall.InotifyAddWatch(int(fd), p, mask)
	}); cerr != nil {
		return 0, cerr
	}
	return wd, werr
}

// addTree adds watches for rel and every eligible directory beneath it.
// Exhausting the kernel watch budget logs and continues — an unwatched
// subtree degrades to rescan freshness, it does not break correctness.
func (w *Watcher) addTree(rel string) error {
	full := w.root
	if rel != "" {
		full = filepath.Join(w.root, filepath.FromSlash(rel))
	}
	return filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		sub := filepath.ToSlash(strings.TrimPrefix(p, w.root))
		sub = strings.TrimPrefix(sub, "/")
		if strings.EqualFold(path.Base(p), ".git") && sub != "" {
			return filepath.SkipDir
		}
		if sub != "" && w.shouldDescend != nil && !w.shouldDescend(sub) {
			return filepath.SkipDir
		}
		wd, aerr := w.addWatch(p)
		if aerr != nil {
			log.Printf("watch: add %s: %v (subtree degrades to rescan freshness)", sub, aerr)
			return filepath.SkipDir
		}
		w.mu.Lock()
		w.dirs[int32(wd)] = sub
		w.mu.Unlock()
		return nil
	})
}

// loop reads and dispatches events until Close wakes it via the poller.
func (w *Watcher) loop() {
	defer close(w.done)
	buf := make([]byte, 64<<10)
	for {
		n, err := w.file.Read(buf)
		if err != nil {
			return // closed (or unrecoverable): the owner's rescan carries on
		}
		for off := 0; off+16 <= n; {
			evMask := binary.LittleEndian.Uint32(buf[off+4:])
			wd := int32(binary.LittleEndian.Uint32(buf[off:]))
			nameLen := int(binary.LittleEndian.Uint32(buf[off+12:]))
			name := ""
			if nameLen > 0 && off+16+nameLen <= n {
				raw := buf[off+16 : off+16+nameLen]
				for i, b := range raw {
					if b == 0 {
						raw = raw[:i]
						break
					}
				}
				name = string(raw)
			}
			off += 16 + nameLen
			w.dispatch(wd, evMask, name)
		}
	}
}

func (w *Watcher) dispatch(wd int32, evMask uint32, name string) {
	if evMask&syscall.IN_Q_OVERFLOW != 0 {
		if w.onOverflow != nil {
			w.onOverflow()
		}
		return
	}
	w.mu.Lock()
	dir, known := w.dirs[wd]
	if evMask&syscall.IN_IGNORED != 0 {
		delete(w.dirs, wd)
	}
	w.mu.Unlock()
	if !known || name == "" {
		return
	}
	rel := name
	if dir != "" {
		rel = dir + "/" + name
	}
	if evMask&syscall.IN_ISDIR != 0 {
		// A new (or renamed-in) directory: watch it and report its
		// contents — files can land before the watch is active.
		if evMask&(syscall.IN_CREATE|syscall.IN_MOVED_TO) != 0 {
			if strings.EqualFold(name, ".git") {
				return
			}
			if w.shouldDescend == nil || w.shouldDescend(rel) {
				if err := w.addTree(rel); err != nil {
					log.Printf("watch: add new dir %s: %v", rel, err)
				}
				w.reportTree(rel)
			}
		}
		return
	}
	w.onChange(rel)
}

// reportTree emits onChange for every file already inside a newly watched
// subtree, closing the create-before-watch race.
func (w *Watcher) reportTree(rel string) {
	full := filepath.Join(w.root, filepath.FromSlash(rel))
	_ = filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		sub := filepath.ToSlash(strings.TrimPrefix(p, w.root))
		w.onChange(strings.TrimPrefix(sub, "/"))
		return nil
	})
}
