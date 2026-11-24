package core

import (
	"errors"
	"io"
)

// RecvStatus turns a Send / CloseSend failure on a client stream into the
// stream's real status.
//
// gRPC returns io.EOF from Send once the SERVER has ended the stream — a
// worker that refused it at open (no admission slot) or mid-upload (over the
// upload cap) — and the reason is then only on Recv. Returning the io.EOF
// would print "EOF" in place of the refusal and its retry advice. Any other
// error is the caller's own and is returned unchanged; a stream that ends
// cleanly after an io.EOF Send leaves the io.EOF as the answer.
func RecvStatus[T any](stream interface{ Recv() (T, error) }, sendErr error) error {
	if !errors.Is(sendErr, io.EOF) {
		return sendErr
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return sendErr
			}
			return err
		}
	}
}
