package audit

// MutationDetail is a scribe workspace-mutation's record body.  A
// single-target op sets Path; a move/copy sets From and To.
type MutationDetail struct {
	Path          string `json:"path,omitempty"` // workspace-relative target
	From          string `json:"from,omitempty"` // move/copy source
	To            string `json:"to,omitempty"`   // move/copy destination
	BytesBefore   int64  `json:"bytesBefore,omitempty"`
	BytesAfter    int64  `json:"bytesAfter,omitempty"`
	FilesTouched  int    `json:"filesTouched,omitempty"`
	LinesAdded    int    `json:"linesAdded,omitempty"`
	LinesRemoved  int    `json:"linesRemoved,omitempty"`
	SHA256After   string `json:"sha256After,omitempty"`
	HasDiff       bool   `json:"hasDiff,omitempty"`       // a diff payload is stored for this opId
	DiffTruncated bool   `json:"diffTruncated,omitempty"` // that payload was capped for size
}
