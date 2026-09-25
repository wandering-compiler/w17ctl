package snapstore

import (
	"bytes"
	"io"
)

// objectMarkers are the statements a dump of a store that HOLDS something
// cannot be without.
//
// Deliberately a small set of creates rather than a parser: the question is
// not "is this valid SQL for this dialect", it is "did anything at all come
// back". `CREATE TABLE` covers postgres and mysql; the others are here so a
// store made only of sequences, types or views is not called empty.
var objectMarkers = [][]byte{
	[]byte("CREATE TABLE"),
	[]byte("CREATE SEQUENCE"),
	[]byte("CREATE TYPE"),
	[]byte("CREATE VIEW"),
	[]byte("CREATE MATERIALIZED VIEW"),
	[]byte("CREATE INDEX"),
	[]byte("CREATE UNIQUE INDEX"),
	[]byte("CREATE FUNCTION"),
	// A store holding only FOREIGN tables counts as holding something on the
	// counter's side (pg_class relkind 'f'), and pg_dump writes this for them.
	// Without the marker the two sides disagreed about such a database and the
	// caller reported a misdirected dump — a false "different database" refusal
	// over a store that was answered correctly by both tools separately.
	[]byte("CREATE FOREIGN TABLE"),
}

// longestMarker is how much tail has to be carried between writes so a marker
// split across two of them is still seen.
var longestMarker = func() int {
	n := 0
	for _, m := range objectMarkers {
		if len(m) > n {
			n = len(m)
		}
	}
	return n
}()

// objectWatch streams a dump through to w while answering one question: did
// anything that CREATES something go past.
//
// Streaming rather than reading the file back, because a dump is as large as
// the database and the check has to cost nothing on the large case — which is
// the healthy one.
//
// # Why this exists
//
// A dump that FAILS is already an error. A dump that succeeds and contains
// nothing was written out as a valid snapshot, and reconcile then wiped the
// store on the strength of it (`the outgoing branch was already snapshotted,
// so the wipe is recoverable`). marb lost six dev databases that way, over
// eleven silently-empty snapshots of 722 bytes each — a dump of a DIFFERENT,
// empty database, because the in-container route matched a container by port
// digits and ignored the DSN's host (#68, fixed in `containerdump`).
//
// This is the belt for that: it does not need anyone to have thought of the
// particular way a dump can come back wrong, only that a snapshot of a store
// holding tables cannot be a file with no tables in it.
type objectWatch struct {
	w    io.Writer
	tail []byte
	seen bool
}

func newObjectWatch(w io.Writer) *objectWatch { return &objectWatch{w: w} }

func (o *objectWatch) Write(p []byte) (int, error) {
	if !o.seen {
		// The tail of the previous write is prepended so a marker straddling
		// the boundary is still found — a dump arrives in whatever chunk sizes
		// the pipe hands over, and a check that missed a split marker would
		// call a healthy snapshot empty at random.
		hay := p
		if len(o.tail) > 0 {
			hay = append(append(make([]byte, 0, len(o.tail)+len(p)), o.tail...), p...)
		}
		for _, m := range objectMarkers {
			if bytes.Contains(hay, m) {
				o.seen = true
				o.tail = nil
				break
			}
		}
		if !o.seen {
			keep := longestMarker - 1
			if len(hay) < keep {
				keep = len(hay)
			}
			o.tail = append(o.tail[:0], hay[len(hay)-keep:]...)
		}
	}
	return o.w.Write(p)
}

// sawObject reports whether anything created an object.
func (o *objectWatch) sawObject() bool { return o.seen }
