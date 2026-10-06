package core

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	consolerpcpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/rpc"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
)

// The codegen worker connection — the SECOND dial a `w17ctl codegen` run makes
// (docs/specs/console/codegen-on-the-cluster.md §6).
//
// The console places the run (PlaceGenerate) and answers with the address of a
// relay's proxy, the SHA-256 of that proxy's certificate and a single-use
// ticket. The GenerateProject stream then goes THERE — through the relay's
// reverse tunnel to a codegen worker serving the compiler's own
// `w17.storage.codegen.CodegenService` — and never through the console.
//
// What this connection deliberately does NOT carry, and why each one matters:
//
//   - No bearer token. The console's credential belongs to the console; a relay
//     and a worker are other machines, and a token presented to them is a token
//     handed to whoever runs them. The ticket is the whole authorisation here,
//     and it is single-use and short-lived by design.
//   - No call recorder. The recorder exists so the `ci-push` role can be
//     checked against the console RPCs a pipeline makes (callrecord.go); the
//     worker is not the console, and a recorded worker call would demand a
//     console permission for something the console never serves.
//
// The pin is TRANSPORT, not lock crypto: it answers "is this the proxy the
// console named?", exactly as a CA would for a domain. Nothing about the lock
// is signed or verified here (D4 §4).

// WorkerTicketHeader is the metadata key the relay's proxy redeems the
// placement ticket from. The relay defines the same string
// (plugins/cluster/src/lib/relayserver.TicketHeader); it is re-declared here
// because w17ctl may not depend on the plugin module (its srcgo closure, and
// any plugin closure, stays 0).
const WorkerTicketHeader = "w17-cluster-ticket"

// workerMaxMessage is the per-message cap on the worker connection, both ways —
// the same 256 MiB the relay's tunnel applies end to end (plugins/cluster
// tunnel.MaxMessage). A generated file set is streamed one op per message, but
// a single pb file or bundle can still exceed gRPC's 4 MiB default.
const workerMaxMessage = 256 << 20

// workerKeepaliveTime is how often the client pings an idle-looking worker
// connection. A GenerateProject run is minutes of server-side work with no
// bytes on the wire, and a relay or tunnel that dies silently (a black-holed
// socket, not a closed one) would otherwise leave the client waiting out the
// whole per-attempt deadline.
//
// Five minutes is gRPC's default server enforcement minimum
// (keepalive.EnforcementPolicy.MinTime): a client pinging more often than the
// server permits is answered with GOAWAY "too_many_pings", which kills the
// very run the ping was protecting. Raise the relay's MinTime before lowering
// this; never the other way round.
const workerKeepaliveTime = 5 * time.Minute

// workerKeepaliveTimeout is how long a ping may go unanswered before the
// connection is declared dead.
const workerKeepaliveTimeout = 20 * time.Second

// Placer is the console's placement call for a codegen run on the cluster.
//
// The generated console client (`consolerpcpb.CodegenClient`) satisfies it by
// method set; tests substitute a fake.
type Placer interface {
	PlaceGenerate(ctx context.Context, in *codegenpb.PlaceGenerateRequest, opts ...grpc.CallOption) (*codegenpb.PlaceGenerateResponse, error)
	// SignAclLock signs an ACL lock the worker computed and could not sign
	// (it holds no key) — a GeneratedOp.sign the run returned.
	SignAclLock(ctx context.Context, in *codegenpb.SignAclLockRequest, opts ...grpc.CallOption) (*codegenpb.SignAclLockResponse, error)
}

// PlacerFn builds the placement client over the console connection DialCodegen
// opened. Tests override it with a fake.
var PlacerFn = consolePlacer

// consolePlacer is the console's PlaceGenerate: the generated console client
// over the connection DialCodegen opened.
func consolePlacer(conn grpc.ClientConnInterface) Placer {
	return consolerpcpb.NewCodegenClient(conn)
}

var _ Placer = (consolerpcpb.CodegenClient)(nil)

// DialWorkerFn is the worker dial seam; production keeps realDialWorker,
// tests point it at an in-process worker.
var DialWorkerFn = realDialWorker

// DialWorker opens the GenerateProject connection a placement granted:
// `address` is the relay proxy, `certFingerprint` the lowercase hex SHA-256 of
// its leaf certificate (the console's answer), `ticket` the single-use grant
// presented as WorkerTicketHeader metadata on every call.
func DialWorker(address, certFingerprint, ticket string) (codegenpb.CodegenServiceClient, *grpc.ClientConn, error) {
	return DialWorkerFn(address, certFingerprint, ticket)
}

func realDialWorker(address, certFingerprint, ticket string) (codegenpb.CodegenServiceClient, *grpc.ClientConn, error) {
	if strings.TrimSpace(address) == "" {
		return nil, nil, errors.New("codegen worker: the placement named no address")
	}
	if strings.TrimSpace(ticket) == "" {
		return nil, nil, errors.New("codegen worker: the placement carried no ticket")
	}
	tlsCfg, err := workerTLSConfig(address, certFingerprint)
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		// The ticket — and ONLY the ticket. No bearerPerRPC, no recorder: see
		// the file comment.
		grpc.WithPerRPCCredentials(workerTicket(ticket)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(workerMaxMessage),
			grpc.MaxCallSendMsgSize(workerMaxMessage),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    workerKeepaliveTime,
			Timeout: workerKeepaliveTimeout,
		}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("codegen worker: dial %s: %w", address, err)
	}
	return codegenpb.NewCodegenServiceClient(conn), conn, nil
}

// workerTLSConfig pins the relay proxy's leaf certificate.
//
// InsecureSkipVerify turns off the CHAIN check, not the identity check: a
// relay's proxy certificate may be self-signed, and the pin — "this exact
// certificate" — is strictly narrower than any CA's "somebody I trust signed
// this". The pin is enforced in BOTH hooks: crypto/tls runs
// VerifyPeerCertificate only on a full handshake, and a resumed TLS 1.3
// session skips it, while VerifyConnection runs on both paths. A pin held in
// the first hook alone holds for the first connection and is silently skipped
// afterwards.
func workerTLSConfig(address, certFingerprint string) (*tls.Config, error) {
	want := normalizeFingerprint(certFingerprint)
	if want == "" {
		// Refused rather than defaulted to "trust anything": an unpinned
		// connection would hand the lock and the whole proto tree to whoever
		// answers at the address.
		return nil, fmt.Errorf("codegen worker: the placement for %s carried no certificate fingerprint to pin", address)
	}
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, //nolint:gosec // G402: the chain check is replaced by the leaf pin both hooks below carry.
		VerifyPeerCertificate: workerPinVerifier(address, want),
		VerifyConnection:      workerPinConnectionVerifier(address, want),
	}, nil
}

// normalizeFingerprint accepts the hex form with or without colons and in any
// case; the console sends lowercase hex.
func normalizeFingerprint(fp string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
}

// leafFingerprint is the lowercase hex SHA-256 of a certificate's DER bytes —
// the form the relay registry stores and the console hands out.
func leafFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// workerPinRefusal opens every pin failure. gRPC reports a failed handshake
// as Unavailable — the code of a network blip — with the verifier's text in
// the message, so this phrase is how the run tells an identity failure (never
// retried) from a lost connection (retried).
const workerPinRefusal = "codegen worker certificate pin refused"

// IsWorkerPinRefusal reports whether err is a refused relay certificate.
func IsWorkerPinRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), workerPinRefusal)
}

// workerPinVerifier checks the LEAF only: a pin satisfied by any certificate
// in the chain would be satisfied by an intermediate, which is not an
// identity.
func workerPinVerifier(address, want string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("%s: %s presented no certificate", workerPinRefusal, address)
		}
		if got := leafFingerprint(rawCerts[0]); got != want {
			return fmt.Errorf("%s: %s presented certificate %s, the console pinned %s — "+
				"this is not the relay the console placed the run on", workerPinRefusal, address, got, want)
		}
		return nil
	}
}

// workerPinConnectionVerifier is the same rule on the hook a RESUMED session
// reaches.
func workerPinConnectionVerifier(address, want string) func(tls.ConnectionState) error {
	verify := workerPinVerifier(address, want)
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("%s: %s presented no certificate", workerPinRefusal, address)
		}
		return verify([][]byte{cs.PeerCertificates[0].Raw}, nil)
	}
}

// workerTicket presents the placement ticket on every call of the worker
// connection. It requires transport security: a ticket over plaintext is a
// ticket anyone on the path can redeem first.
type workerTicket string

func (t workerTicket) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{WorkerTicketHeader: string(t)}, nil
}

func (workerTicket) RequireTransportSecurity() bool { return true }
