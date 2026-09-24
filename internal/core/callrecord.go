package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc"
)

// RecordConsoleCallsEnv names the file every console RPC this process makes is
// appended to. Unset (the default) records nothing and costs nothing.
//
// # Why this exists
//
// The `ci-push` role is an allow-list, deliberately: for a credential nobody
// is watching, an endpoint that did not exist when the role was written must
// be DENY. The cost is that the list is written by hand, and it has been
// short FIVE times — four found by a consumer's pipeline going red, and the
// fifth found here, having never turned anything red at all: `push` stamps a
// migration with the branch's initiative, the refused lookup was folded into
// "initiative lookup failed", and the push then SUCCEEDED, unstamped. An
// unstamped migration is one no freeze can collapse, and a pipeline would
// have produced them indefinitely while reporting green.
//
// # Why recording and not inference
//
// Inference was built and measured. Take the packages a command imports,
// transitively, and collect the console RPCs called there: it runs, and it
// found the fifth gap. It is not shippable at that granularity — from `push`
// the closure reaches all of `internal/storageclient`, so it reports fourteen
// candidates with one real among them. A gate whose output is mostly entries
// the reader must dismiss teaches the reader to dismiss it, and the real
// finding arrives inside that noise. Precision there means a call graph, not
// a bigger regex.
//
// So: record. The recorded set is exactly what the role must cover, with no
// inference and no deny list for anyone to maintain. It also records the
// branches a CI run takes that a developer's does not — which is where the
// fifth gap lived (`push` with no `--initiative`, resolving from the branch
// name), and is the one thing a static reading of the code cannot know.
const RecordConsoleCallsEnv = "W17_RECORD_CONSOLE_CALLS"

// recordedCallKey is `<Service>/<Method>` — the pair the CONSOLE receives.
//
// Taken from the wire's own `/<proto package>.<Service>/<Method>`, not from
// the client-side seam that produced it. The console names its permissions
// `<module>.<Service>.<Method>`, and the module is a property of the console's
// domain layout rather than of anything the client can see — so the client
// records the half it can know for certain, and the mapping to a permission id
// is done where the console's own generated gate can be read (see
// `srcgo/tests/cirole`).
//
// A `public_fqn` surface (ProjectRegistry is served as
// `w17.registry.ProjectRegistry`) changes the PACKAGE, never the service name
// or the method, so keying on those two survives the public mirror.
func recordedCallKey(fullMethod string) string {
	trimmed := strings.TrimPrefix(fullMethod, "/")
	slash := strings.LastIndex(trimmed, "/")
	if slash < 0 {
		return ""
	}
	svcFQN, method := trimmed[:slash], trimmed[slash+1:]
	if method == "" {
		return ""
	}
	svc := svcFQN
	if dot := strings.LastIndex(svcFQN, "."); dot >= 0 {
		svc = svcFQN[dot+1:]
	}
	if svc == "" {
		return ""
	}
	return svc + "/" + method
}

var callRecordMu sync.Mutex

// recordConsoleCall appends one call to the recording file.
//
// Best effort, and deliberately so: this is an observation channel, never a
// behaviour. A recording that cannot be written must not fail the command the
// pipeline is actually running — the alternative is a diagnostic that can take
// production down.
//
// Appended (not buffered and flushed at exit) because the interesting runs are
// the ones that FAIL partway: the fifth gap was a refusal swallowed mid-command,
// and a recorder that only writes on a clean exit would have missed the call
// that mattered. One line per call, duplicates included — the reader dedupes,
// and a count is occasionally the thing worth seeing.
func recordConsoleCall(fullMethod string) {
	path := os.Getenv(RecordConsoleCallsEnv)
	if path == "" {
		return
	}
	key := recordedCallKey(fullMethod)
	if key == "" {
		return
	}
	callRecordMu.Lock()
	defer callRecordMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, key)
}

// recordUnaryCalls / recordStreamCalls are dial options EVERY console dial
// installs unconditionally. Each is a no-op while
// [RecordConsoleCallsEnv] is unset — `recordConsoleCall` returns on the empty
// path before touching anything.
//
// Installed unconditionally on purpose. A conditional
// `append(opts, recordingOptions()...)` at five call sites is five places to
// forget one, and the seam that gets forgotten is the one nothing tests — so
// the recorder would report a clean set for the surface nobody looks at, which
// is the exact failure mode this whole mechanism exists to remove. Two
// interceptor frames and one getenv per RPC is not a cost a CLI can measure.
//
// STREAMS are recorded too, and that is not completeness for its own sake:
// `CompileIR` is a stream, it is the call `push` cannot proceed without, and it
// was one of the two missing from the very first version of the role. A
// recorder that saw only unary calls would report a clean set for the command
// whose gap started this.
func recordUnaryCalls() grpc.DialOption {
	return grpc.WithChainUnaryInterceptor(func(
		ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
	) error {
		// Recorded BEFORE the call, so a refusal is recorded too. The question
		// the role answers is "which permissions does this command DEMAND", and
		// a call that came back PermissionDenied is the clearest possible
		// instance of demanding one.
		recordConsoleCall(method)
		return invoker(ctx, method, req, reply, cc, opts...)
	})
}

func recordStreamCalls() grpc.DialOption {
	return grpc.WithChainStreamInterceptor(func(
		ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
		streamer grpc.Streamer, opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		recordConsoleCall(method)
		return streamer(ctx, desc, cc, method, opts...)
	})
}
