package core

import (
	"errors"
	"io"

	"google.golang.org/grpc"

	applyfetchpb "github.com/wandering-compiler/sdk/go/pb/applyfetch"
	consolerpcpb "github.com/wandering-compiler/sdk/go/pb/consoleapi/rpc"
	codegenpb "github.com/wandering-compiler/sdk/go/pb/w17compiler"
	w17registrypb "github.com/wandering-compiler/sdk/go/pb/w17registry"
)

// consolerpcpb.CodegenClient (the console rpc-gateway's re-hosted codegen
// surface, `w17lock.console.rpc.Codegen`) carries the SAME method set + signatures
// as the compiler's codegenpb.CodegenServiceClient — it re-declares the WHOLE
// CodegenService surface and reuses the identical codegenpb (w17compiler) types
// — so it satisfies that interface by type identity. This compile-time assertion
// is what lets DialCodegen return the gateway client transparently: every w17ctl
// codegen consumer keeps its codegenpb.CodegenServiceClient seam unchanged while
// dialing console-app (the legacy cmd/console daemon is retired).
var _ codegenpb.CodegenServiceClient = (consolerpcpb.CodegenClient)(nil)

// The console rpc-gateway's re-hosted deploy-registry clients carry the SAME
// method sets + signatures as the public w17registrypb / applyfetchpb clients
// (the facade re-declares the whole surface reusing those types), so they
// satisfy the public interfaces by type identity — letting DialProjectRegistry
// / DialMigrationFetch / DialFixtureFetch return the gateway client
// transparently. This is what moves `w17ctl push` / `fetch` / `migrate apply`
// / `init` / `fixtures` onto console-app and retires the legacy cmd/console
// daemon (its last surface).
var (
	_ w17registrypb.ProjectRegistryClient = (consolerpcpb.ProjectRegistryClient)(nil)
	_ applyfetchpb.MigrationFetchClient   = (consolerpcpb.MigrationFetchClient)(nil)
	_ applyfetchpb.FixtureFetchClient     = (consolerpcpb.FixtureFetchClient)(nil)
)

// DialCodegenFn is a package var production code keeps at the real
// Dial; tests override it to inject a bufconn-backed client for
// in-process verification.
var DialCodegenFn = realDialCodegen

// DialCodegen dials the console's codegen surface at addr — the console-app
// rpc gateway's re-hosted `w17lock.console.rpc.Codegen` (console runs the
// compiler; G-selfhost-codegen). The returned client is typed as the compiler's
// codegenpb.CodegenServiceClient (the gateway client satisfies it by identity),
// so every consumer (codegen.Run, schema push, plan, compat, verify, plugin,
// lock edit) targets the console with no per-site change.
func DialCodegen(addr string) (codegenpb.CodegenServiceClient, *grpc.ClientConn, error) {
	return DialCodegenFn(addr)
}

func realDialCodegen(addr string) (codegenpb.CodegenServiceClient, *grpc.ClientConn, error) {
	// The codegen response carries every generated file in one message
	// (handlers, bundles, AND the full pb stub set(s)), comfortably
	// exceeding gRPC's 4 MiB default receive cap. Lift it so large
	// projects don't trip ResourceExhausted on the client.
	conn, err := grpc.NewClient(ConsoleTarget(addr),
		// Always TLS (dev console is fronted by a TLS-terminating proxy).
		// See ConsoleTransportCreds.
		ConsoleTransportCreds(addr),
		// Attach the logged-in bearer (no-op when not logged in). See auth.go.
		grpc.WithPerRPCCredentials(bearerPerRPC{addr: addr}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(codegenMaxRecvMsgSize)),
	)
	if err != nil {
		return nil, nil, err
	}
	// The gateway client (re-hosted Codegen) — NOT the native
	// codegenpb.NewCodegenServiceClient. It satisfies codegenpb.CodegenServiceClient
	// (see the assertion above), so the return type is unchanged.
	return consolerpcpb.NewCodegenClient(conn), conn, nil
}

// DialMigrationFetchFn is a package var production code keeps at the real
// Dial; tests override it to inject a fake/bufconn-backed MigrationFetch
// client for in-process verification.
var DialMigrationFetchFn = realDialMigrationFetch

// DialMigrationFetch dials the console's public MigrationFetch service at
// addr. This is the apply-path fetch surface w17ctl uses instead of the
// private registry client — it keeps w17ctl's apply commands free of any
// console/client (registrypb) dependency.
func DialMigrationFetch(addr string) (applyfetchpb.MigrationFetchClient, *grpc.ClientConn, error) {
	return DialMigrationFetchFn(addr)
}

func realDialMigrationFetch(addr string) (applyfetchpb.MigrationFetchClient, *grpc.ClientConn, error) {
	// Mirror DialCodegen: a fetch response carries every migration body up to
	// the pinned target, which can exceed gRPC's 4 MiB default cap on a large
	// schema history, so lift the receive cap to the same generous bound.
	conn, err := grpc.NewClient(ConsoleTarget(addr),
		// Always TLS (dev console is fronted by a TLS-terminating proxy).
		// See ConsoleTransportCreds.
		ConsoleTransportCreds(addr),
		// Attach the logged-in bearer (no-op when not logged in). See auth.go.
		grpc.WithPerRPCCredentials(bearerPerRPC{addr: addr}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(codegenMaxRecvMsgSize)),
	)
	if err != nil {
		return nil, nil, err
	}
	// Gateway-rehosted MigrationFetch (satisfies applyfetchpb.MigrationFetchClient
	// — see the assertion above), NOT the native applyfetchpb client.
	return consolerpcpb.NewMigrationFetchClient(conn), conn, nil
}

// DialFixtureFetchFn is a package var production code keeps at the real
// Dial; tests override it to inject a fake/bufconn-backed FixtureFetch
// client for in-process verification.
var DialFixtureFetchFn = realDialFixtureFetch

// DialFixtureFetch dials the console's public FixtureFetch service at
// addr. This is the apply-path fixture-seed fetch surface w17ctl uses
// instead of the private registry client — it keeps w17ctl's fixture
// apply command free of any console/client (registrypb) dependency.
func DialFixtureFetch(addr string) (applyfetchpb.FixtureFetchClient, *grpc.ClientConn, error) {
	return DialFixtureFetchFn(addr)
}

func realDialFixtureFetch(addr string) (applyfetchpb.FixtureFetchClient, *grpc.ClientConn, error) {
	// Mirror DialCodegen: a fixture-seed response carries every rendered upsert
	// statement for the named fixture, which can exceed gRPC's 4 MiB default cap
	// on a large seed set, so lift the receive cap to the same generous bound.
	conn, err := grpc.NewClient(ConsoleTarget(addr),
		// Always TLS (dev console is fronted by a TLS-terminating proxy).
		// See ConsoleTransportCreds.
		ConsoleTransportCreds(addr),
		// Attach the logged-in bearer (no-op when not logged in). See auth.go.
		grpc.WithPerRPCCredentials(bearerPerRPC{addr: addr}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(codegenMaxRecvMsgSize)),
	)
	if err != nil {
		return nil, nil, err
	}
	// Gateway-rehosted FixtureFetch (satisfies applyfetchpb.FixtureFetchClient).
	return consolerpcpb.NewFixtureFetchClient(conn), conn, nil
}

// DialProjectRegistryFn is a package var production code keeps at the real
// Dial; tests override it to inject a fake/bufconn-backed ProjectRegistry
// client for in-process verification.
var DialProjectRegistryFn = realDialProjectRegistry

// DialProjectRegistry dials the console's public ProjectRegistry service at
// addr — the irpb-free authoring surface (register / push schema + fixtures /
// list history) w17ctl uses instead of the private console/client
// (registrypb) wrapper. Keeps w17ctl's push/registry commands free of any
// registrypb / irpb / fixturespb / planpb dependency (public-split Block 3).
func DialProjectRegistry(addr string) (w17registrypb.ProjectRegistryClient, *grpc.ClientConn, error) {
	return DialProjectRegistryFn(addr)
}

func realDialProjectRegistry(addr string) (w17registrypb.ProjectRegistryClient, *grpc.ClientConn, error) {
	// A PushSchema request carries the whole compiled-IR blob, which can exceed
	// gRPC's 4 MiB default cap on a large schema, so lift both send + receive
	// caps to the same generous bound.
	conn, err := grpc.NewClient(ConsoleTarget(addr),
		// Always TLS (dev console is fronted by a TLS-terminating proxy).
		// See ConsoleTransportCreds.
		ConsoleTransportCreds(addr),
		// Attach the logged-in bearer (no-op when not logged in). See auth.go.
		grpc.WithPerRPCCredentials(bearerPerRPC{addr: addr}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(codegenMaxRecvMsgSize),
			grpc.MaxCallSendMsgSize(codegenMaxRecvMsgSize),
		),
	)
	if err != nil {
		return nil, nil, err
	}
	// Gateway-rehosted ProjectRegistry (satisfies w17registrypb.ProjectRegistryClient).
	return consolerpcpb.NewProjectRegistryClient(conn), conn, nil
}

// DialAuthServiceFn is a package var production code keeps at the real Dial;
// tests override it to inject a bufconn-backed AuthService client.
var DialAuthServiceFn = realDialAuthService

// DialAuthService dials the console's auth surface (the rpc gateway's re-hosted
// `w17lock.console.rpc.AuthService`) at addr — the ONE gRPC endpoint w17ctl
// logs in through. SignIn is unauthenticated (it mints the bearer); ListMyOrgs
// is bearer-gated, so callers thread the freshly minted token via per-call
// metadata. The dial therefore attaches NO stored bearer (unlike the other
// console dials) — login must work before any token is stored.
func DialAuthService(addr string) (consolerpcpb.AuthServiceClient, *grpc.ClientConn, error) {
	return DialAuthServiceFn(addr)
}

func realDialAuthService(addr string) (consolerpcpb.AuthServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.NewClient(ConsoleTarget(addr),
		// Always TLS (dev console is fronted by a TLS-terminating proxy). See
		// ConsoleTransportCreds. NO bearerPerRPC — see the doc above.
		ConsoleTransportCreds(addr),
	)
	if err != nil {
		return nil, nil, err
	}
	return consolerpcpb.NewAuthServiceClient(conn), conn, nil
}

// codegenMaxRecvMsgSize caps the codegen response the client will accept
// (256 MiB) — orders of magnitude above any realistic generated-file
// payload, while still bounding a runaway server.
const codegenMaxRecvMsgSize = 256 << 20

// RecvGeneratedFiles drains a generator RPC's server stream, handing each file
// to fn as it arrives, and reports how many arrived.
//
// The file-producing codegen RPCs stream one GeneratedFile per message instead
// of returning one batched response, because a batch puts a project's WHOLE
// generated output inside a single gRPC message — and the console's
// gateway→backend hop dials without call options, so it runs on gRPC's default
// 4 MiB receive cap. That made project size a correctness limit, not a
// performance one. Callers must consume each file where it arrives rather than
// re-accumulating the stream, or the cap simply moves back into the client.
//
// A truncated run ends with an ERROR, never a clean io.EOF, so "fewer files
// than the project has" is never a silent outcome — the count comes back with
// the error so the caller can say how far it got.
func RecvGeneratedFiles(stream grpc.ServerStreamingClient[codegenpb.GeneratedFile], fn func(*codegenpb.GeneratedFile) error) (int, error) {
	n := 0
	for {
		f, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if err := fn(f); err != nil {
			return n, err
		}
		n++
	}
}
