package core

import codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"

// ChunkBudget is how many bytes of files one request message carries.
//
// Well under gRPC's 4 MiB default so the cap never decides anything, and
// large enough that a big tree is tens of messages rather than thousands —
// each message costs a round of framing, and the point of chunking is the
// ceiling, not throughput.
const ChunkBudget = 1 << 20

// SendProtoChunks streams a proto tree in budget-sized batches.
//
// `mk` builds one request message around a batch and `send` puts it on the
// wire, so the same loop serves every file-carrying request without this
// package knowing any of their shapes.
//
// A file larger than the budget goes in a message of its own: splitting a
// FILE would need the server to reassemble it, which is a second protocol for
// no gain — the cap is 4 MiB and no proto file approaches it.
//
// Why any of this exists: a unary request carries the whole tree in ONE gRPC
// message, and a receiver's default cap is 4 MiB. Measured at ~101k lines of
// proto the tree is 4.16 MB and the call is refused outright — no
// degradation, no warning, one line further and codegen stops working.
func SendProtoChunks[T any](
	files []*codegenpb.ProtoFile,
	mk func([]*codegenpb.ProtoFile) T,
	send func(T) error,
) error {
	var batch []*codegenpb.ProtoFile
	var budget int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := send(mk(batch)); err != nil {
			return err
		}
		batch, budget = nil, 0
		return nil
	}
	for _, f := range files {
		if len(batch) > 0 && budget+len(f.GetContents()) > ChunkBudget {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, f)
		budget += len(f.GetContents())
	}
	return flush()
}
