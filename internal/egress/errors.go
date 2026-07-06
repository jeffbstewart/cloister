package egress

import (
	"errors"

	"github.com/jeffbstewart/cloister/internal/egress/wire"
)

// Refusal sentinels.  Each maps to an audit decision when the subsystem is
// wired to the sink: ErrDenied → rejected_denied, ErrInternalHost →
// rejected_internal, ErrSearchCap/ErrExtractCap → rejected_cap,
// ErrNeedsApproval → pending_approval.  ErrUnknownHandle is a model/logic
// error (the loop handed back a token we never minted), never a gate.
// Callers compare with errors.Is.
var (
	ErrDenied        = errors.New("egress: host is on the deny list")
	ErrInternalHost  = errors.New("egress: target resolves to an internal/loopback host")
	ErrSearchCap     = errors.New("egress: daily search cap reached")
	ErrExtractCap    = errors.New("egress: daily extract cap reached")
	ErrUnknownHandle = errors.New("egress: unknown or expired retrieval handle")
	ErrNeedsApproval = errors.New("egress: raw-URL extract requires operator approval")
	ErrNotHTTPS      = errors.New("egress: only https targets are permitted")

	// ErrResponseTooBig is raised by the wire leaf; re-exported so consumers
	// compare against egress sentinels only.
	ErrResponseTooBig = wire.ErrResponseTooBig
)
