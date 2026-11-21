# w17ctl — the wandering-compiler developer CLI

`w17ctl` is the **single client and entrypoint** for a w17 project. Everything a
developer *or a pipeline* does — scaffolding, change requests, codegen, applying
migrations, running the local stack — goes through it.

```
w17ctl <command> --help     # context-sensitive help for any command
```

**Status: alpha (v0).** Any release may carry a breaking fix; stay current
with `w17ctl update` (the binary) and `w17ctl update --all` (the project's
sdk/go, codegen version and plugins too).

## Get access

w17 is invitation-only. A w17 operator sends you a personal invitation, which
creates **your own organization** with you as its owner. Open the invitation
link (`https://w17.app/invite?token=…`), register at the console, then sign in
from the terminal with `w17ctl login` (below). After that you can create as
many projects as you like and invite other people into your organization. There
is no password reset — only a password change while signed in — so keep your
password and second factor safe. No invitation yet? Ask the person who gave you
access.

## Prerequisites

- **Docker** (with `docker compose`) — the local stack, `stack build` and
  `test` run in it.
- A shell on linux or macOS (amd64 or arm64).
- Not root, or installing from a shell with no terminal (a coding agent's, a CI
  step)? Install into a directory you can write — `--dir ~/.local/bin` — because
  the installer can only fall back to `sudo` where it can ask for a password.

## Install

```sh
curl -fsSL https://get.w17.dev/install.sh | sh -s -- --pre
```

Installs into `/usr/local/bin`, verifying the download against the release's
`SHA256SUMS` first — always, with no flag to skip it.

`--pre` is needed **while w17ctl is in release candidates**: `latest` means the
newest STABLE release, and there is not one yet. Drop it once v0.1.0 ships.

```sh
curl -fsSL https://get.w17.dev/install.sh | sh -s -- --pre --dir ~/.local/bin      # no root needed
curl -fsSL https://get.w17.dev/install.sh | sh -s -- --version <tag> --dir ~/.local/bin
```

Or take a `.tar.gz` straight from [Releases](https://github.com/wandering-compiler/w17ctl/releases)
— linux and macOS, amd64 and arm64. There is nothing to configure after
unpacking: `w17ctl` is a single static binary and keeps its state in `~/.w17/`.

```sh
w17ctl version           # what you have, and which console it defaults to
w17ctl version --check   # …and whether a newer w17ctl release exists
w17ctl update            # upgrade in place, no install command to paste
w17ctl update --check    # inside a project: is w17ctl, sdk/go or any plugin behind?
w17ctl login             # sign in to the console (a person's step — see below)
```

### Upgrading

`w17ctl update` replaces this binary with the newest release, into the
directory it is already running from. You do not have to remember the install
line, and you do not have to know which tag is newest.

It **invokes the installer above** rather than resolving the release itself,
which is the point rather than an implementation detail: one place knows which
release is newest, and that place already verifies the download against
`SHA256SUMS`. Prereleases are included automatically while your binary is
itself a prerelease — which is every binary today, since `latest` means the
newest stable release and there is not one yet. `--stable` opts out,
`--version <tag>` pins, `--dry-run` only reports what would be installed.
`--check` reports what is behind — this binary, and inside a project its sdk/go
and plugins — and `--all` updates all of it. After an update, run
`w17ctl guide --force` in each project: `AGENTS.md` ships inside w17ctl.

`w17ctl version --check` asks only about the binary, without changing anything.
It is a flag rather than part of `version` because it costs a network call, and
`version` has to keep working with no network — it is what you run when
something is already wrong.

`w17ctl version` prints the console address compiled into the binary. That is
only the LAST resort in the resolution order — `--console`, then
`W17_CONSOLE_ADDR`, then the console you are logged into, then this default — so logging in is what decides where
commands go.

## Start here — you + a coding agent

w17 is built to be driven by a **coding agent** (Claude Code, …). Two steps:

**1. Log in once — yourself, not the agent.** Login is a person's act (a
browser or code sign-in plus a second factor); an agent cannot complete it.

```
w17ctl login                    # a browser on this machine
w17ctl login --method code      # over ssh / in a remote workspace: open the URL anywhere, paste the code back
w17ctl whoami                   # who you are, and your organizations
```

The password sign-in (`--method password`, or `--email` + `W17_PASSWORD`) asks
for the second-factor code when the console wants one; `--code` or
`W17_MFA_CODE` supplies it up front.

**2. Paste this to your coding agent ONCE to onboard it** (it's context, not a
task — the point is that the agent learns how to find things, so every later
session already knows the drill and you can just ask for features):

```text
This project is built with w17 (wandering-compiler): a declarative compiler where
you declare schemas, DB models, queries, mutations, events, and APIs as proto +
annotations, and it generates the gRPC binaries, DB layer, SQL migrations,
gateways, and clients — the only hand-written code is the business-handler bodies.

Do this now:
1. Run `w17ctl guide`. It writes AGENTS.md (+ a CLAUDE.md importing it) and
   w17/specs/, the platform reference. Read AGENTS.md — it is short.
2. Do NOT read w17/specs/ whole (it is large). Navigate it by topic:
     grep -i '<keyword>' w17/specs/INDEX.md          # find topics
     w17ctl guide topic <id>                         # print one topic
     sed -n '<first>,<last>p' w17/specs/<file>       # same, without w17ctl
     grep -n -A12 '<annotation>' w17/specs/annotations.md   # annotation lookup
   In a new project, read w17/specs/start-choosing.md and start-layout.md once.
3. Never run `w17ctl login` — ask me when a command says you are not logged in.

From now on I'll just ask you to implement features. Always follow the w17 way:
declare then generate, hand-write only handler bodies, look the annotation or
topic up in w17/specs/ when unsure, and report w17 bugs or doc gaps with
`w17ctl report`. Do not invent DB or gRPC plumbing by hand.
```

That's it — from then on you just describe features. The platform reference is
regenerated by the compiler, so it always matches your w17 version.
**Everything below is reference detail you'll rarely need to read.**

## Mental model — what w17ctl is (and isn't)

w17ctl is **deliberately dumb**. It holds *no* compiler know-how. Every real
decision — compiling the proto IR, planning SQL migrations, running the
compatibility engine, signing the lock — happens on the **console** (the
backend), reached over a gRPC API. w17ctl's whole job is three things:

1. **Transport** — talk to the console's gRPC API (always over TLS).
2. **Local disk** — read/write the project's files: the signed `w17/lock.yaml`,
   generated code, snapshots, and the dev-machine config under `~/.w17/`.
3. **Docker** — drive the local compose stack.

```
   you / CI  ──▶  w17ctl  ──▶  console gRPC API  ──▶  the compiler core
              (transport +      (all the know-how)     (private IP)
               disk + docker)
        ◀──  finished artifacts the client writes verbatim  ──
```

So when a command "generates code" or "plans a migration", w17ctl is **uploading
your inputs and applying what the console sends back** — it never compiles
anything itself. Your `.proto` files are therefore uploaded to the console, and
the console keeps them: they are the schema history migrations are planned
from. This is why the client is small, publishable, and safe to run in
CI: it can't produce a broken or forged artifact, only apply a console-signed one.

w17ctl finds your project by walking parent directories to the nearest one
holding `w17/`, so you can run it from anywhere inside the tree.

## State it keeps

| Location | What | Notes |
|---|---|---|
| `w17/lock.yaml` | the **signed** project lock — connections, targets, generated-code config, pinned schema | edited by scaffolding commands; signed by the console |
| `~/.w17/auth.yaml` | console credentials (bearer + org memberships) per console | written by `login` |
| `~/.w17/device-<hash>.key` | this machine's device key for one console (PKCS #8 PEM, mode 0600; refused when others can read it) — signs DPoP proofs for consoles that bind devices | made by the first `login` to that console; `W17_DPOP_KEY` names one file for every console |
| `~/.w17/config.yaml` | dev-machine project registry + per-project host-port assignments + presets | written by `init` / `project` |
| `w17/certs/`, `w17/tmp/<initiative>/snapshots/` | local dev PKI + branch schema snapshots | `certs` / `db` / `initiative` |

### The certificate committed in this repo

`internal/core/devca.crt` is a **dev CA certificate**, and it is meant to be
here. Only the public half ships — the private key is dev-only and never
leaves the monorepo — and the client trusts it for **loopback dials only**, so
that `w17ctl login localhost:…` can verify your local dev terminator with no
environment set up. It grants nothing against a remote console: a dial to
anything but loopback goes through the system trust store like any other TLS
client.

---

## Command reference

### Getting started

| Command | What it does · under the hood |
|---|---|
| `guide` | Write `AGENTS.md` — the short guide a coding agent keeps in context (golden rules, the work loop, how to navigate the reference; client-embedded, works offline) — plus a `CLAUDE.md` containing `@AGENTS.md` (Claude Code reads `CLAUDE.md`; an existing one is never overwritten, only hinted at), **and** fetch `w17/specs/` (the platform reference, generated server-side so it always matches the compiler). `--stdout` prints the guide; `--force` refreshes `AGENTS.md`; `--no-specs` skips the server fetch, which needs a reachable console and a login but no organization. |
| `guide topic <id>` / `guide topics [query]` | Print one topic of `w17/specs/` by its id, or list the `w17/specs/INDEX.md` lines matching a substring (all when empty). Offline; an unknown id exits non-zero with the closest ids. |

| `version [--check]` | Print this binary's version, commit, build date and compiled-in console address. `--check` also asks the installer which release is newest and says whether you are behind — one network call, and it reports rather than fails when there is none. |
| `update` | Upgrade this binary in place, into the directory it runs from. Invokes `install.sh` rather than resolving the release itself, so the download is verified against `SHA256SUMS` and there is only one implementation of "which release is newest". `--stable` refuses prereleases, `--version <tag>` pins, `--dir` targets elsewhere, `--dry-run` reports without installing. `--check` reports what is behind (inside a project also sdk/go and plugins); `--all` updates all of it. |
| `report <kind>` | Tell w17 what is wrong, missing or unclear: `report bug\|docs\|feature\|security --title … -F body.md` (`--critical` for a blocking bug, `--topic` for docs); `report list` / `show <id>` / `withdraw <id>` follow it. Kept and triaged by the console; you are mailed when it ships. |

### Auth & organizations
Talk to a console and pick your org scope. Login offers three sign-ins and
asks which in a terminal (`--method` picks one): the console's own page in a
browser on this computer (OAuth 2.1 with a loopback redirect and PKCE —
w17ctl never sees the password); the same page in a browser on any device,
which then shows a code to paste into w17ctl (for w17ctl over ssh or in a
remote workspace); and email + password in the terminal, a single gRPC call
to the console — email + password in, bearer out.

| Command | What it does · under the hood |
|---|---|
| `login [<host>]` | Sign in and store the bearer + org memberships in `~/.w17/auth.yaml`. In a terminal it asks how; `--method browser\|code\|password` answers, and without a terminal (a script) it is `browser`. **`browser` — a browser on this computer:** fetches the console's OAuth metadata (`/.well-known/oauth-authorization-server`), registers w17ctl once per console (the `client_id` is kept under `oauth_clients` in `auth.yaml`), prints and opens the sign-in URL, waits up to 5 minutes for the redirect to `http://127.0.0.1:<random port>/callback`, and exchanges the code with a DPoP proof of this machine's device key. **`code` — a browser on any device:** the same, except that nothing listens here and nothing is opened: the redirect is the console's code page (`https://<issuer>/oauth/code`), which after the approval shows `code#state` to paste into w17ctl (its state is checked as the redirect's would be; the code lasts 5 minutes). In a terminal the pasted code shows as `*`, and `c` pressed before anything is typed copies the sign-in URL to the clipboard (OSC 52, so over ssh too; inside tmux it needs `set -g allow-passthrough on` or `set -g set-clipboard on`); a paste is never taken for the key, even one starting with `c`. The console offers it when its auth plugin sets `oauth_code_redirect`. The issuer is `https://<console host>` (the gRPC port dropped: `grpcs://api.w17.app:50051` → `https://api.w17.app`); `--issuer` or `W17_CONSOLE_OAUTH_ISSUER` names another. **`password` (or `--no-browser`) — the terminal:** `AuthService.SignIn` over gRPC, prompting for anything not given (email → password (hidden) → host); `--email`, `--password` or `W17_PASSWORD` select it as well, and `--email` + `W17_PASSWORD` keep it non-interactive. A second factor is asked for when the console wants one; `--code` or `W17_MFA_CODE` supplies it (an authenticator code, a recovery code, or the one the console e-mails). |
| `console list` / `console use <url>` | The consoles you are logged into, and which one is active. `login` makes the console it authenticated against active; `use` moves it back. |
| `logout [<url>]` | Drops a console's stored credential from `~/.w17/auth.yaml`. |
| `key public [<console>]` | Prints a device key's public half as a JWK on stdout (making the key first if there is none): the file `W17_DPOP_KEY` names, else this machine's key for the console (default: the active one). It is what the console needs to issue a machine account's token. Stderr names the file and its thumbprint (the device id the console shows). |
| `whoami` | Prints the stored identity + orgs for the active console (`--all` = every logged-in console), and the session expiry when one is known (with a warning once it has passed). A local read; `--verify` asks the console whether it still accepts the credential, and a `W17_TOKEN` run always asks. |
| `org list` / `org use <slug>` | List the orgs you belong to (the stored list; `--refresh` asks the console) and pick the default; the default org scopes subsequent commands. |
| `org pin <slug>` | Writes a minimal `w17/lock.yaml` (`org_id`, `org_slug`, `w17_url`, nothing else) in a directory that is **not** a project, e.g. a plugins repository. Every w17ctl run there then acts in that organization, a `W17_TOKEN` run included. Refuses inside a project, whose lock is the console's. |

**Unattended callers don't log in.** CI and a production deployment have no
terminal to run `login` on and no home directory worth writing a credential
into. They set two variables instead:

| Variable | |
|---|---|
| `W17_TOKEN` | an API token minted for a **machine account** (console → Organizations → your org → Machine accounts), not a person's password. Takes precedence over a stored login. |
| `W17_CONSOLE_ADDR` | the console the token was minted for — required unless it is the console the binary was built for (see below). |

The second is what makes the first safe: a token is presented only to the
console it is bound to, never to a host named by a `--console` flag. Without
the binding it is presented nowhere — which arrives as `invalid credentials`,
so w17ctl warns on stderr when a token is set and then not sent. (A build with
a compiled-in `DefaultConsoleAddr` — every released binary — uses that when
`W17_CONSOLE_ADDR` is unset.)

Nothing here is required locally: with no token set, the credential store
answers exactly as before.

**The organization comes from the lock.** A token carries no organization of
its own, and an account in several organizations is refused on every org-scoped
call that names none (`missing permission …`): the console will not guess.
Inside a project, the lock's `org_id` decides. w17ctl translates it to the
slug the `w17-org` header carries, asking the console (`ListMyOrgs`, once per
run) when there is no login cache to do it, which is always the case for
`W17_TOKEN`. A lock naming an organization the account is not in stops the
command with an error that names both. The lock's organization is translated through
the login cache wherever that cache knows the id, and asked of the console only
when its `w17_url` names the console being dialed; never beside an explicit
`--org`. `project register` and `init`, which replace the lock, act in the
organization they register under for every call. A directory that is not a project, such
as a plugins repository where `plugin sign` / `plugin gen` run, gets its
organization from `w17ctl org pin <slug>`: run it once, signed in as a person,
and commit the file. `plugin sign <dir>` and `plugin gen <dir>` read the lock
above `<dir>` first, then the working directory's.

**Device-bound sessions (DPoP).** A console whose auth binds devices ties every
token to a key and accepts it only with a proof (RFC 9449) signed by that key,
so a token copied off a machine is useless without the key file.

- `login` signs its sign-in with this machine's key for that console,
  `~/.w17/device-<hash>.key` (the hash is of the dialed console's origin), made on the
  first login to it. One key per console: a console that revokes its key
  cannot take another console's session with it. When the console binds the
  session, `auth.yaml` records the key's thumbprint beside the token
  (`device:`) and every later call carries `Authorization: DPoP <token>` plus a
  fresh proof. A console that binds nothing gets plain `Bearer`, as before;
  w17ctl learns which from the console's answer to the sign-in, not from its
  version.
- A key file in `~/.w17` that other users can read or write is refused
  (`chmod 600 <file>`). A key `W17_DPOP_KEY` names is refused only when others
  could replace it (group- or world-writable), so the usual secret mounts work:
  a Kubernetes Secret volume (mode 0644 by default; set `defaultMode: 0400` if
  you can) or a swarm secret under `/run/secrets` (0444).
- Proofs are signed for `https://<host>[:<port>]` of the address you dial (port
  443 left out), and the console checks that against the origin it is
  configured with. A console reachable under two names (`localhost` and
  `127.0.0.1`, say) accepts only the one its configuration names. Dial the
  console by the name its configuration uses, or set
  `W17_CONSOLE_DPOP_ORIGIN` to its origin; w17ctl says so when this happens.
  The override changes only the origin proofs are signed for — the key file is
  chosen by the address you dial, so setting it never switches keys.
- **Revoked device:** the console refuses a revoked key for good. w17ctl deletes
  that console's key file and says to `login` again, which makes a new key —
  the console sees a new device. Deleting a key file yourself does the same.
- **An older w17ctl** rewriting `auth.yaml` (any `login`, `org use`, …) drops the
  `device:` field it does not know. The bound session then goes out as Bearer
  and the console refuses it (`DEVICE_PROOF_REQUIRED`): log in again with the
  current w17ctl.
- **Machine accounts (CI, deployments):** `W17_TOKEN` is a machine account's
  token, and on a console that binds devices it is issued for the account's
  public key. Make the key once with `W17_DPOP_KEY=./ci-device.key w17ctl key
  public` and issue the token for the printed JWK. A machine account has
  exactly one key: every job and replica using its token shares that key file,
  and a token for a different key is refused (`BOT_KEY_MISMATCH`) until the
  account's device is revoked. Give the job `W17_DPOP_KEY` (the key file, kept
  secret like the token) beside `W17_TOKEN` and `W17_CONSOLE_ADDR`; `W17_TOKEN`
  is sent with a proof only when `W17_DPOP_KEY` is set. w17ctl never deletes a
  key named by `W17_DPOP_KEY`; when it is revoked, make a new key and issue a
  new token for it. The CI configs codegen writes (`w17/ci/`, and the
  `.github/workflows/w17-*.yaml` of `w17ctl ci`) take the key file's content
  — the PEM, not the JWK — from the secret `W17_DEVICE_KEY` beside `W17_TOKEN`,
  write it to a private file and point `W17_DPOP_KEY` at it. Leave the secret
  unset until the console binds devices: a console that does not refuses a
  signed call.
- **`W17_DPOP_KEY` names a file that is not there** (with `W17_TOKEN`): the
  call is refused here, and the fix is to point the variable at the key the
  token was issued for — not `login`, which makes a session of its own.

### Project setup & scaffolding
These **edit the signed lock** and write starter proto files. No codegen — they
prepare inputs.

| Command | What it does · under the hood |
|---|---|
| `init` | Wizard: prompts for project name + generated-code paths, **registers the project with the console you are logged into**, writes a fresh signed `w17/lock.yaml`, and auto-registers it in `~/.w17/config.yaml`. Refuses if a lock exists. Every prompt has a flag; with no terminal, `w17ctl init --name <p> --stubs-root srcgo/gen --language go --proto-dir proto --languages-dir w17/languages --languages en --e2e yes --ci github --skip-connections` asks nothing (`--org` when you belong to several organizations). |
| `connection <cmd>` | Wizard to add a DB/store connection to the lock (identity = connection name). Edits the signed lock. |
| `domain <cmd>` / `module <cmd>` | Scaffold the `proto/domains/<domain>[/<module>]/w17.proto` cascade sentinel; `--with-example` seeds the four-layer (queries / mutations / business / types) shape. Pure local file writes. |
| `template [<surface>]` | Prints a commented example `.proto` for one surface (domain, module, types, queries, mutations, business, rest, admin, events, subscribers, rpc, mcp, cli). A "how do I annotate X" reference; `--list` to enumerate. |
| `target <cmd>` | Declare **what** codegen emits + **how** it deploys — the lock's `generated_code` entries: `client` (FE clients), `grpc-client` (Go client pkg), `business`, `binary` (composed binaries), `ci`, `scale` (prod replicas). All edit the signed lock. |
| `plugin <cmd>` | List the console's plugin catalogue; `install` one (e.g. `auth`) into the project — stages its proto tree (activating it is naming it in `(w17.domain).plugins`); `update`, and for plugin authors `dev` / `gen` / `sign` / `render`. |
| `secrets <cmd>` | Production-secrets (age tier): `init` mints a project age keypair; `encrypt` turns a plain `.secrets` into a committable `.secrets.age`. Runtime decrypts when a key is present, reads plain otherwise — seamless dev, strong devops. Local crypto only. |
| `secrets set\|unset\|list\|pull\|open\|exec\|fetch` | An infrastructure environment's values, held by the console (no files in git): `set --env <env> SERVICE KEY…` seals each value to a write code from the project's page (every form, `--dir` and stdin included, merges: keys not written keep their values; `unset` removes one); `list` shows each key — a secret as set or empty, a variable with its value — with no read code, `--check` against the stack; `pull` reads one service's values back after a read code (`--seal-to` keeps them age-encrypted, `open` / `exec` use such a file); `fetch --env <env> --out DIR` is the deploy's read — as a `deploy-fetch` machine account (`W17_TOKEN`) with Deploy access to the environment, checked against every `.env.example`. |
| `ci init\|update\|show\|remove` | The GitHub automation of the declared infrastructure: codegen then writes `.github/workflows/w17-ci.yaml`, `w17-build.yaml` and `w17-deploy-<env>.yaml` (prunes only its own). `show` prints what to set up in GitHub. |
| `deploy --env <env> [--with-tests]` | One person's MVP/PoC shortcut: mint + pin migrations, codegen, fetch the environment's values from the console, build the images, then — for a swarm environment — ship them over SSH (no registry) and deploy blue/green on its server, or — for aws-ecs / gcp-cloudrun — apply its Terraform (`--state-bucket`). Always warns that it is not the recommended way. |
| `infra <cmd>` | Where and how the project is deployed — swarm (your own servers), aws-ecs or gcp-cloudrun: environments, edge, TLS, backups, server setup. `init` / `update` prompt and write `w17/infra.yaml`; `show` / `remove`; the next codegen renders `deploy/<env>/`. |
| `client <cmd>` | Generated gRPC clients of THIRD-PARTY APIs (REST from OpenAPI, SOAP from WSDL): `generate` / `update` / `list` / `remove`, under `proto/clients/<name>/`. |
| `sdk update` / `codegen-version [<v>]` | Move the project onto a new sdk/go release (no local Go toolchain needed) / pin the codegen engine it generates with (a minor, or an exact release candidate). `update --all` does both. |
| `certs` | Fill a directory with a local dev PKI (self-signed CA + CA-signed leaf) for the internal-mesh TLS switch (`W17_INTERNAL_TLS=on`). Idempotent — never overwrites. |

### Codegen & schema
The compiler-driven commands. Each **uploads your protos to the console** and
writes back what it returns.

| Command | What it does · under the hood |
|---|---|
| `codegen` | Generate **all** derived code: Storage gRPC handlers + the ACL / eventbus / MCP bundles. Uploads every `.proto` under `proto/` to the console, which compiles the IR + emits the files; w17ctl writes them under `<root>/<gen_dir>/`. |
| `verify` | Re-checks that every committed generated lock (ACL + eventbus) still matches the current proto — the CI drift hook paired with `codegen`. Asks the console; non-zero exit on drift. |
| `push` | Push everything (schema + fixtures + future artifact types) to the console in one call; the server diffs + stores. Every changed input MINTS a new stored migration — a CI step, not a dev loop (see the migrations guide). Builds the whole project; `--proto <file>` narrows it to those files and their imports, and a table that build leaves out is planned as a DROP the console refuses until `--decide <table>:table_drop=needs_confirm` confirms it. A directory is not accepted as `--proto`. |
| `migrate generate` | Compile + push the schema; **the console plans the SQL migrations** (initial create vs. revision diff, auto-detected) and pins the lock target. `migrate list` shows history; `migrate push-raw` is the hand-authored-body escape hatch. To APPLY it, your generated server binary owns the fetch + apply step — w17ctl plans migrations, the binary that owns the database applies them. |
| `fixtures <cmd>` | Apply console-rendered fixture seeds (parameterized upserts) to a connection's target store (DSN via `W17_TARGET_<CONN>` env). |
| `admission` | Read the console's codegen admission-gate counters (queue depth, decisions, wait times, cost-model drift). A console read; takes no project. |
| `clean` | Remove codegen output (pb stubs / FE clients / languages / generated bundle files) while **preserving hand-written packages**. |

### Schema lifecycle — the review flow (PR analog)
Snapshots + a compatibility engine + a review/merge gate. Git branch → initiative
(main/master → trunk).

| Command | What it does · under the hood |
|---|---|
| `initiative <cmd>` | Initiatives + snapshots: `current` / `list` / `show` / `materialize` / `push`. A git branch maps to an initiative; the first push lazily materializes it. Schema state lives in the console's generated storage. |
| `compat <cmd>` | Compatibility **report** between two snapshots — DB / WIRE / API / EVENT findings + severity — with a dev/prod policy gate. `report --base --head`, or `against-trunk`. The console's compat engine does the analysis; w17ctl renders the verdict. |
| `review <cmd>` | `open` / `show` / `approve` / `merge` a set of schema changes. Merging into a **prod-mode** env runs compat and **blocks BREAKING changes** (exit non-zero); dev warns + proceeds. |
| `env <cmd>` | Deployment environments + mode (dev/prod). A prod-mode env turns the compat engine's BREAKING verdict into a hard merge block. |

### Local development
Docker + local project ergonomics. This is where w17ctl drives compose.

| Command | What it does · under the hood |
|---|---|
| `stack <cmd>` | Local docker-compose lifecycle: `build` / `up` / `down` / `logs` / `ps` / `restart`. `build` SYNCS the stores to your protos — it reads what each database holds, the console plans the difference, the client applies it; `--no-build` does only that. `up` runs the same sync after starting the containers (`--no-sync` to skip). A sync that would DROP a table or column, or RETYPE one, is refused with the list: `--lossy=apply` does it, `--lossy=snapshot` keeps a copy first (dumped inside the store's container when this machine has no `pg_dump`). Injects the project's unique host ports from `~/.w17/config.yaml`. |
| `project <cmd>` | Dev-machine project registry: `list` / `import` / `remove` / `ports` / `ps` + run presets. Keeps **unique host ports across all installed projects** (no env-file juggling) in `~/.w17/config.yaml`. |
| `db snapshot <cmd>` | Local dev DB snapshots scoped to the current initiative: `db snapshot save` / `list` / `activate` / `delete`. Branch-scoped checkpoints for fast switching. |
| `test` | Run the generated e2e suite against a throwaway stack of its own: brings the project's compose up under a unique project name with dynamic host ports, syncs the stores, runs the suite as an external HTTP/MCP client, and tears the stack down (`--keep` leaves it running for debugging). |
| `stress <target>` | Load-test a deployed gateway with the generated stress presets (each preset's `stress.yaml` plan); prints throughput/latency and fails on a breached threshold — usable as a CI load gate. |
| `ui <cmd>` | Serve the local web app — a human-facing view + controller over the project registry, presets, and Docker stacks (the GUI counterpart to `project` / `stack`). `127.0.0.1` only, no image; `ui start` runs it in the background and `ui stop` stops it. |

---

## Why the split matters (design invariant)

The compiler's IP and all trust-bearing crypto (lock signing) live on the
console, never in the client. w17ctl ships as a **standalone public tool** and
imports **zero** private compiler code — only the public `sdk/go` module + the
console's gRPC contracts. That's what lets the same minimal client both scaffold
a project on your laptop and gate a production release in CI: it verifies + applies
console-signed artifacts, and can't forge one.

### This repository is an export

The sources here are generated from the private monorepo where w17ctl is
developed, which is why you will find no `_test.go` files and comments that
speak of test seams ("indirected so tests can…"). The tests exist; they live
beside the code they cover, in the monorepo, and **they run there before every
release** — the same pipeline also builds this exported tree and fails the
release if it does not compile with zero private compiler code in its import
graph.

So nothing here is untested, but nothing here is testable by you either.
If you hit a bug, report it with `w17ctl report bug` — a description that
reproduces it is worth as much to us as a patch would be: we turn it into a regression test in the tree where the test
actually runs.
