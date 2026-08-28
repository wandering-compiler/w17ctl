// Command w17ctl is the developer-side tool for the w17 platform —
// the single CLI a developer drives the whole toolchain through.
// Its job is the things the user can't do by hand: code generation
// (`codegen`) + drift verification (`verify`), migration planning +
// push to the console (`migrate`, `push`), project + proto scaffolding
// (`init`, `domain`, `module`, `template`), the signed-lock connection
// declaration (`connection`) + the `target` subtree of codegen/deploy
// declarations (client / grpc-client / business / binary / ci / scale),
// secrets + dev TLS (`secrets`, `certs`), the e2e suite (`test`), and
// the local docker-compose stack
// (`stack`, `clean`). Anything a developer can express in proto stays
// in proto — w17ctl never wraps proto edits.
//
// Architectural distinction:
//   - `w17migrate` runs in deploy environments; single-purpose;
//     api_token auth; reads w17/lock.yaml + applies migrations.
//   - `w17ctl` runs on developer machines / CI; broad surface;
//     orchestrates platform interactions + keeps the signed lock
//     consistent and synced with the console.
//
// main.go is pure wiring: the command tree + dispatch live in the cmd root
// package (cmd/root.go, which registers every cmd/<command> subpackage); this
// entrypoint only hands it os.Args.
package main

import (
	"os"

	"github.com/wandering-compiler/w17ctl/cmd"
)

func main() {
	cmd.Run(os.Args[1:])
}
