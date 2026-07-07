package scribe

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jeffbstewart/cloister/internal/audit"
	"github.com/jeffbstewart/cloister/internal/workspace"
)

// permit_non_utf8 edit path: read the file as raw bytes, view it as Latin-1
// (byte↔code-point), run the SAME engine on that view, re-encode, and —
// because a non-UTF-8 edit is not cleanly reviewable — ALWAYS route it
// through human approval.  This is how the em-dash bug (a lone Windows-1252
// 0x97 that breaks UTF-8) gets repaired.

// readRaw reads an existing regular file without the UTF-8 requirement, size-capped.
func (s *Server) readRaw(p workspace.Path) ([]byte, os.FileInfo, *readErr) {
	fi, err := os.Lstat(p.String())
	if err != nil {
		return nil, nil, &readErr{decError, fmt.Errorf("file does not exist")}
	}
	if fi.IsDir() {
		return nil, nil, &readErr{decError, fmt.Errorf("path is a directory")}
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, &readErr{decError, fmt.Errorf("not a regular file")}
	}
	if fi.Size() > workspace.MaxTextFileBytes {
		return nil, nil, &readErr{decOverflow, fmt.Errorf("file %d bytes exceeds the %d cap", fi.Size(), workspace.MaxTextFileBytes)}
	}
	data, err := os.ReadFile(p.String())
	if err != nil {
		return nil, nil, &readErr{decError, err}
	}
	return data, fi, nil
}

// finishNonUTF8 records the summary then previews (dryRun) or ALWAYS stages the
// change pending approval — a permit_non_utf8 write is never applied unattended.
func (s *Server) finishNonUTF8(rec audit.Record, p workspace.Path, oldRaw, finalBytes []byte, perm os.FileMode, viewDiff string, dryRun bool, notify func(string)) *mcp.CallToolResult {
	if string(finalBytes) == string(oldRaw) {
		rec.Decision = decNoChange
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": rec.Mutation.Path, "status": "no_change", "changed": false})
	}
	added, removed := diffStat(viewDiff)
	rec.Mutation.BytesBefore = int64(len(oldRaw))
	rec.Mutation.BytesAfter = int64(len(finalBytes))
	rec.Mutation.FilesTouched = 1
	rec.Mutation.LinesAdded = added
	rec.Mutation.LinesRemoved = removed
	rec.Mutation.SHA256After = sha256hex(finalBytes)
	if dryRun {
		rec.Decision = decDryRun
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": rec.Mutation.Path, "diff": viewDiff, "dryRun": true, "permitNonUtf8": true})
	}
	payload, _ := diffPayload("", viewDiff)
	return s.awaitApproval(rec, stagedOp{OpID: rec.RunID, Tool: rec.Tool, Path: s.rel(p), Content: finalBytes, Perm: uint32(perm), Payload: payload}, notify)
}

func (s *Server) applyDiffRepair(rec audit.Record, p workspace.Path, diff string, dryRun bool, notify func(string)) *mcp.CallToolResult {
	raw, fi, rerr := s.readRaw(p)
	if rerr != nil {
		return s.rejected(rec, rerr.decision, rerr.err)
	}
	viewOld := workspace.Latin1Decode(raw)
	newView, err := workspace.ApplyDiff([]byte(viewOld), diff)
	if errors.Is(err, workspace.ErrAlreadyApplied) {
		rec.Decision = decNoChange
		s.audit(rec)
		return jsonResult(map[string]any{"opId": rec.RunID, "path": rec.Mutation.Path, "status": "already_applied", "changed": false})
	}
	if err != nil {
		return s.rejectDiff(rec, decError, err, diff)
	}
	viewDiff := workspace.Unified("a/"+rec.Mutation.Path, "b/"+rec.Mutation.Path, []byte(viewOld), newView, workspace.DefaultContext)
	return s.finishNonUTF8(rec, p, raw, workspace.BytesFromView(string(newView)), fi.Mode().Perm(), viewDiff, dryRun, notify)
}

func (s *Server) replaceStringRepair(rec audit.Record, p workspace.Path, find, replace, scope string, expected *int, dryRun bool, notify func(string)) *mcp.CallToolResult {
	raw, fi, rerr := s.readRaw(p)
	if rerr != nil {
		return s.rejected(rec, rerr.decision, rerr.err)
	}
	viewOld := workspace.Latin1Decode(raw)
	matches := strings.Count(viewOld, find)
	if matches == 0 {
		return s.rejected(rec, decError, fmt.Errorf("find string not found"))
	}
	if expected != nil && *expected != matches {
		return s.rejected(rec, decError, fmt.Errorf("expectedCount %d but `find` occurs %d times", *expected, matches))
	}
	viewNew := viewOld
	if scope == "all" {
		viewNew = strings.ReplaceAll(viewOld, find, replace)
	} else {
		viewNew = strings.Replace(viewOld, find, replace, 1)
	}
	viewDiff := workspace.Unified("a/"+rec.Mutation.Path, "b/"+rec.Mutation.Path, []byte(viewOld), []byte(viewNew), workspace.DefaultContext)
	return s.finishNonUTF8(rec, p, raw, workspace.BytesFromView(viewNew), fi.Mode().Perm(), viewDiff, dryRun, notify)
}

func (s *Server) replaceRegexRepair(rec audit.Record, p workspace.Path, re *regexp.Regexp, replacement, scope string, expected *int, dryRun bool, notify func(string)) *mcp.CallToolResult {
	raw, fi, rerr := s.readRaw(p)
	if rerr != nil {
		return s.rejected(rec, rerr.decision, rerr.err)
	}
	view := []byte(workspace.Latin1Decode(raw))
	locs := re.FindAllIndex(view, -1)
	if len(locs) == 0 {
		return s.rejected(rec, decError, fmt.Errorf("pattern not found"))
	}
	if expected != nil && *expected != len(locs) {
		return s.rejected(rec, decError, fmt.Errorf("expectedCount %d but pattern matches %d times", *expected, len(locs)))
	}
	var newView []byte
	if scope == "all" {
		newView = re.ReplaceAll(view, []byte(replacement))
	} else {
		loc := re.FindSubmatchIndex(view)
		expanded := re.Expand(nil, []byte(replacement), view, loc)
		newView = append(append(append([]byte{}, view[:loc[0]]...), expanded...), view[loc[1]:]...)
	}
	viewDiff := workspace.Unified("a/"+rec.Mutation.Path, "b/"+rec.Mutation.Path, view, newView, workspace.DefaultContext)
	return s.finishNonUTF8(rec, p, raw, workspace.BytesFromView(string(newView)), fi.Mode().Perm(), viewDiff, dryRun, notify)
}
