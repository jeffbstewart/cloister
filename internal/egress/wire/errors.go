package wire

import "errors"

// ErrResponseTooBig reports an upstream body over the configured cap.  It
// lives in wire (not the core egress package) because doCapped raises it and
// the search/extract providers compare against it; callers use errors.Is.
var ErrResponseTooBig = errors.New("egress: upstream response exceeds the configured cap")
