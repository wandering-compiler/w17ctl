package target

// Cmd is the `w17ctl target <kind> …` parent — the editors for
// the lock's `generated_code` declarations. Every subcommand mutates a
// `generated_code.*` slot of the SIGNED lock (re-signing on save), so
// they share one section: client (clients[]), grpc-client
// (grpc_clients[]), business (business_bundles[]), binary (binaries[]),
// ci (ci_configs[]), scale (replicas[]).
//
// These are code-AGNOSTIC project settings — WHAT codegen emits + HOW
// it deploys — not API surface (that's proto). They live in the lock
// because the lock is signed for codegen reproducibility (every dev
// shares the same one); the signature is why they need a command rather
// than a hand-edit.
type Cmd struct {
	Client     ClientCmd     `cmd:"" help:"Generated-client (clients[]) entries — wizard appends an FE client (framework + language + output_root + wire). Re-signs on save."`
	GrpcClient GrpcClientCmd `cmd:"" name:"grpc-client" help:"grpc_clients[] entries — declare the Go gRPC clients package codegen renders (the seam a hand-written business binary builds its downstream clients from). Re-signs on save."`
	Business   BusinessCmd   `cmd:"" help:"business_bundles[] entries — declare the <domain>-business bundle codegen emits (the facade tier the project's RegisterBusiness plugs into; required for a gateway+storage+business -server). Re-signs on save."`
	Binary     BinaryCmd     `cmd:"" help:"Composed-binary (binaries[]) entries — fold a domain's generated components (gateway ± admin, or the full gateway+storage+business -server) into one binary. Re-signs on save."`
	Ci         CiCmd         `cmd:"" help:"CI-config (ci_configs[]) entries — opt a CI provider (github|gitlab|circleci|azure|bitbucket|jenkins|generic) into generated e2e CI under w17/ci/<provider>/. Re-signs on save."`
	Scale      ScaleCmd      `cmd:"" help:"Per-service PROD replica counts (replicas[]) — set/list/unset how many replicas a deployable bundle runs. A <domain>-storage bundle past 1 materializes the contract-blind storage gRPC proxy in prod. Re-signs on save."`
}
