package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// Ctx carries the template substitutions every
// generated .proto file needs. Project is the proto-safe
// package prefix (`-` replaced with `_`); Domain + Module are
// the operator-supplied identifiers.
type Ctx struct {
	Project string // proto-safe package prefix (e.g. "my_saas")
	Domain  string // lowercase ident (e.g. "users")
	Module  string // lowercase ident; empty when rendering a domain-only template
}

// ProtoSafePackagePrefix returns the project name reshaped
// into a valid proto package identifier — lowercase letters,
// digits, and underscores. Dashes (common in project names
// like `my-saas`) become underscores; anything else gets
// dropped. Two consecutive separators collapse.
//
// The result feeds the `package <prefix>.<domain>[.module];`
// header in every generated file. Operators preferring a
// shorter aesthetic prefix (`e2e` instead of `e2e_project`)
// edit the generated w17.proto by hand — a future
// `Lock.proto_package_prefix` field would lift this into
// configuration.
func ProtoSafePackagePrefix(project string) string {
	var b strings.Builder
	prev := byte(0)
	for i := 0; i < len(project); i++ {
		c := project[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			prev = c
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
			prev = c + 32
		case c == '-' || c == '_' || c == ' ':
			if prev != '_' && b.Len() > 0 {
				b.WriteByte('_')
				prev = '_'
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "project"
	}
	return out
}

// identRe is the validator both `domain add` and
// `module add` apply to their positional arguments. Matches
// the conservative subset that's safe as a proto package
// identifier AND a directory name on every platform: lowercase
// ASCII letter followed by lowercase letters, digits, or
// underscores.
var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidateIdent refuses identifiers that wouldn't compile
// as a proto package segment or wouldn't be safe as a
// directory name. Returns the steering message inline so
// the caller can wrap it with command context.
func ValidateIdent(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if !identRe.MatchString(value) {
		return fmt.Errorf("%s name %q must start with a lowercase letter and contain only lowercase letters, digits, or underscores (matches both proto package + directory rules)",
			kind, value)
	}
	return nil
}

// RenderTemplate executes one of the const-string templates
// below with the given scaffold context. Trailing newline
// guaranteed.
func RenderTemplate(name, body string, ctx Ctx) ([]byte, error) {
	tpl, err := template.New(name).Parse(body)
	if err != nil {
		return nil, fmt.Errorf("scaffold template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("scaffold render %s: %w", name, err)
	}
	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Skeleton templates — what `domain add` and `module add` produce
// without `--with-example`. Minimal: only the w17.proto sentinel that
// the cascade loader recognises.
// ---------------------------------------------------------------------------

const DomainSkeletonProto = `syntax = "proto3";

// Domain-level cascade config for {{.Domain}}.
//
// Every .proto under proto/domains/{{.Domain}}/<module>/<...>/*.proto
// inherits the (w17.module) option below — the connection +
// cascade defaults declared here apply to every table in the
// domain. Per-module overrides land in
// proto/domains/{{.Domain}}/<module>/w17.proto.
//
// File is a SENTINEL — options only, no messages.

package {{.Project}}.{{.Domain}};

import "w17/module.proto";

option (w17.module) = {
  connection: {
    name: "{{.Domain}}-postgres",
    dialect: POSTGRES,
    version: "18"
  }
};
`

// DomainWithExampleProto extends the bare skeleton with the
// channels[] + event_defaults blocks the example events.proto
// references. Without these the example's (w17.event)
// annotations would point at undeclared channels.
const DomainWithExampleProto = `syntax = "proto3";

// Domain-level cascade config for {{.Domain}}.
//
// Every .proto under proto/domains/{{.Domain}}/<module>/<...>/*.proto
// inherits the (w17.module) option below — connection +
// channel catalogue + event defaults declared here apply to
// every table / event in the domain. Per-module overrides
// land in proto/domains/{{.Domain}}/<module>/w17.proto.
//
// File is a SENTINEL — options only, no messages.

package {{.Project}}.{{.Domain}};

import "w17/module.proto";

option (w17.module) = {
  connection: {
    name: "{{.Domain}}-postgres",
    dialect: POSTGRES,
    version: "18"
  },

  // Eventbus channels — every (w17.event) in this domain
  // names one of these. Two channels split by durability:
  //
  //   notes      — NATS JetStream for durable lifecycle
  //                events. Default retry (5 deliveries /
  //                30s ack / exponential backoff); per-event
  //                retry overrides for important signals.
  //   telemetry  — in-process MEMORY for best-effort fire-
  //                and-forget. Drops on full buffer; no
  //                redelivery; one delivery attempt.
  channels: [
    {
      name: "notes"
      transport: TRANSPORT_NATS
      retry: {
        max_deliver: 5
        ack_wait_seconds: 30
        backoff: BACKOFF_EXPONENTIAL
      }
      drain_timeout_seconds: 30
    },
    {
      name: "telemetry"
      transport: TRANSPORT_MEMORY
      buffer_size: 4096
      retry: { max_deliver: 1 }
      drain_timeout_seconds: 5
    }
  ],

  // Per-event retry default. Events that name a channel
  // without their own retry block inherit this — keeps the
  // (w17.event) annotations terse for typical signals.
  event_defaults: {
    retry: { max_deliver: 3 }
  }
};
`

const ModuleSkeletonProto = `syntax = "proto3";

// Module-level cascade for {{.Domain}}.{{.Module}}.
//
// Without an (w17.module) override block this file is purely a
// declaration anchor — the module inherits everything from
// proto/domains/{{.Domain}}/w17.proto. Add options here only
// when this module needs a different connection / dialect /
// cascade defaults than the domain root declares.
//
// File is a SENTINEL — options only, no messages.

package {{.Project}}.{{.Domain}}.{{.Module}};
`

// EmptyEventsProto — bare events registry. No events
// declared; codegen treats this as "module has no eventbus
// surface yet". Operator adds (w17.event)-bearing messages
// when ready.
const EmptyEventsProto = `syntax = "proto3";

// Eventbus event registry for {{.Domain}}.{{.Module}}.
//
// Each event is a proto message carrying an (w17.event)
// annotation that names its channel + topic; channels
// referenced here must exist in the domain root's w17.proto
// cascade (the channels[] block under option (w17.module)).
// Empty file = no events declared; the codegen skips the
// eventbus surface for this module entirely.
//
// Run "w17ctl module add <domain>/<module> --with-example"
// against a throwaway name to see a fully populated events
// registry.

package {{.Project}}.{{.Domain}}.{{.Module}};
`

// (There is deliberately no EmptySubscribersProto. `subscribers*.proto`
// is a reserved sentinel filename: the eventbus parser hard-fails on one
// carrying no `(w17.event_subscribers)` option, and again on one outside
// the domain root — so a bare per-module skeleton was a file that could
// only ever break the next codegen. The subscriber surface is opt-in via
// `w17ctl template subscribers`, which teaches the placement.)

// ---------------------------------------------------------------------------
// Example payloads — what `--with-example` lays down in addition to
// the skeleton. Self-contained around a fictional `Note` model so
// operators get a working four-layer module (queries / mutations /
// business / types) without having to invent something to populate.
//
// Generated tree (relative to the module root) when `--with-example`
// runs against `module add <DOMAIN>/<MODULE>`:
//
//   <MODULE>/
//     w17.proto                       (skeleton — already produced)
//     types/models.proto              (ExampleTypesProto)
//     queries/note_query.proto        (ExampleQueryProto)
//     mutations/note_mutation.proto   (ExampleMutationProto)
//     business/notes_business.proto   (ExampleBusinessProto)
//     events/events.proto             (ExampleEventsProto)
//
// The example is scoped to what codegens cleanly as a self-contained
// unit: the four core layers + declared events. The consumer-side
// surfaces with cross-cutting prerequisites — subscribers (need coherent
// event-handler business RPCs) and admin (needs the auth plugin) — are
// deliberately NOT scaffolded here; `w17ctl template subscribers|admin`
// shows their shape.
//
// `domain add <NAME> --with-example` produces the domain sentinel
// plus an `example/` module populated with these files.
// The fictional module name keeps the example obviously deletable.
// ---------------------------------------------------------------------------

const ExampleTypesProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}}.{{.Module}};

import "google/protobuf/timestamp.proto";
import "w17/db.proto";
import "w17/field.proto";

// Note — one example DB-backed model. Every column the
// generator creates corresponds to a proto field below;
// the (w17.field) / (w17.db.column) annotations carry
// per-column hints the proto type alone cannot express
// (PK identity, indexes, uniqueness, validation rules).
message Note {
  // (w17.db.table) lets the operator override the default
  // pluralised table name. Drop the option to accept the
  // default (here it would still be "notes").
  option (w17.db.table) = { name: "notes" };

  // PK + autoincrement. pk:true + default_auto:IDENTITY is
  // the most common shape — equivalent to BIGSERIAL on
  // postgres.
  int64 id = 1 [(w17.field) = { pk: true, default_auto: IDENTITY }];

  // Foreign key to a (hypothetical) users table. Replace
  // "users.User" with the actual fully-qualified proto type
  // name. deletion_rule:CASCADE removes notes when the
  // owning user is deleted; switch to RESTRICT to refuse.
  int64 owner_id = 2 [
    (w17.field)     = { related_name: "notes" },
    (w17.db.column) = { fk: "users.User", deletion_rule: CASCADE, index: true }
  ];

  // String column with a length cap + non-empty validation.
  // (w17.field).max_len caps the column at VARCHAR(200);
  // min_len enforces the runtime validator.
  string title = 3 [(w17.field) = { type: CHAR, min_len: 1, max_len: 200 }];

  // TEXT column (no length cap) for the body.
  string body = 4 [(w17.field) = { type: TEXT }];

  // Auto-stamped creation timestamp. The storage tier writes
  // it on INSERT; immutable:true refuses any UPDATE attempt.
  google.protobuf.Timestamp created_at = 5 [(w17.field) = { default_auto: NOW, immutable: true }];
}
`

const ExampleQueryProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}}.{{.Module}};

import "w17/db.proto";
import "w17/field.proto";
import "domains/{{.Domain}}/{{.Module}}/types/models.proto";

message GetNoteReq { int64 note_id = 1; }

message ListNotesReq {
  // proto3-optional → presence-bearing. The DQL "?owner_id"
  // marker skips the predicate when the bind is null, so
  // omitting the filter returns every note.
  optional int64 owner_id = 1;

  // limit clamps the result; the validator rejects values
  // outside 1..100 at the gateway boundary.
  int32 limit = 2 [(w17.field) = { type: NUMBER, gte: 1, lte: 100 }];
}

message ListNotesResp {
  repeated Note notes = 1;
}

// NoteQuery — read side. Lives under queries/ so FE / data
// teams can extend reads without touching mutation surface.
// Every method carries (w17.db.method).query — the storage
// codegen turns these into SQL handlers.
service NoteQuery {
  rpc GetNote(GetNoteReq) returns (Note) {
    option (w17.db.method) = {
      ops: { name: "main", dql: "SELECT n.id, n.owner_id, n.title, n.body, n.created_at FROM {{.Module}}.Note n WHERE n.id = :note_id" }
    };
  }

  rpc ListNotes(ListNotesReq) returns (ListNotesResp) {
    option (w17.db.method) = {
      ops: {
        name: "main",
        dql:
          "SELECT n.id, n.owner_id, n.title, n.body, n.created_at "
          "FROM {{.Module}}.Note n "
          "WHERE ?n.owner_id = :owner_id "
          "ORDER BY n.id DESC "
          "LIMIT :limit"
      }
    };
  }
}
`

const ExampleMutationProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}}.{{.Module}};

import "w17/db.proto";
import "w17/field.proto";
import "domains/{{.Domain}}/{{.Module}}/types/models.proto";

message CreateNoteReq {
  int64  owner_id = 1;
  string title    = 2 [(w17.field) = { type: CHAR, min_len: 1, max_len: 200 }];
  string body     = 3 [(w17.field) = { type: TEXT }];
}

message UpdateNoteReq {
  int64  note_id = 1;
  string title   = 2 [(w17.field) = { type: CHAR, min_len: 1, max_len: 200 }];
  string body    = 3 [(w17.field) = { type: TEXT }];
}

message DeleteNoteReq  { int64 note_id = 1; }
message DeleteNoteResp {}

// NoteMutation — write side. Lives under mutations/ so
// BE-only gates can apply to write surface without
// blocking query authoring.
service NoteMutation {
  rpc CreateNote(CreateNoteReq) returns (Note) {
    option (w17.db.method) = {
      ops: {
        name: "main",
        dql:  "INSERT INTO {{.Module}}.Note SET owner_id = :owner_id, title = :title, body = :body RETURNING id, owner_id, title, body, created_at"
      }
    };
  }

  rpc UpdateNote(UpdateNoteReq) returns (Note) {
    option (w17.db.method) = {
      ops: {
        name: "main",
        dql:  "UPDATE {{.Module}}.Note SET title = :title, body = :body WHERE id = :note_id RETURNING id, owner_id, title, body, created_at"
      }
    };
  }

  rpc DeleteNote(DeleteNoteReq) returns (DeleteNoteResp) {
    option (w17.db.method) = {
      ops: {
        name: "main",
        dql:  "DELETE FROM {{.Module}}.Note WHERE id = :note_id"
      }
    };
  }
}
`

// ExampleDomainRestProto — domain-level REST registry that
// wires the example module's RPCs to HTTP URLs. REV-017
// moves `(w17.http)` off individual RPCs into this central
// `(w17.rest_api)` registry per domain; one file per
// domain, the gateway codegen reads this and emits the
// REST → gRPC routing layer.
const ExampleDomainRestProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}};

import "w17/rest.proto";

// REV-017 — domain-level REST registry. gRPC service
// definitions stay a pure behaviour contract; this file is
// the single source of truth for which methods are exposed
// at which URLs. FastAPI / Django mental model: this is the
// urls.py of the {{.Domain}} domain.
option (w17.rest_api) = {
  name:        "public",
  version:     "v1",
  prefix:      "/api/v1",
  description: "Example REST surface for the {{.Domain}} domain — generated by w17ctl domain add --with-example.",

  groups: [
    {
      prefix:      "/notes",
      description: "Example CRUD endpoints for the Note resource.",
      endpoints: [
        // Storage-tier read exposed directly — the gateway
        // forwards GET /api/v1/notes/{note_id} straight to
        // {{.Module}}.NoteQuery.GetNote without a facade hop.
        { ref: "{{.Module}}.NoteQuery.GetNote",   method: GET,  path: "/{note_id}" },
        { ref: "{{.Module}}.NoteQuery.ListNotes", method: GET,  path: "" },

        // Mutations exposed through their storage RPC too —
        // production setups usually proxy these through the
        // hand-written NotesService facade so business rules
        // (validation, audit, rate-limit) fire first.
        { ref: "{{.Module}}.NoteMutation.CreateNote", method: POST,   path: "" },
        { ref: "{{.Module}}.NoteMutation.UpdateNote", method: PATCH,  path: "/{note_id}" },
        { ref: "{{.Module}}.NoteMutation.DeleteNote", method: DELETE, path: "/{note_id}" }
      ]
    }
  ]
};
`

// ExampleEventsProto — eventbus event registry under the
// module. Three events span the typical shape range:
// lifecycle (default retry), high-importance (retry
// override), and telemetry (in-process MEMORY channel).
// Channels referenced here are declared at the DOMAIN
// root's w17.proto — `domain add --with-example` lays
// them down; bare `domain add` does not, so this file's
// header steers operators to add the cascade themselves
// if needed.
const ExampleEventsProto = `syntax = "proto3";

// Eventbus events for {{.Domain}}.{{.Module}}.
//
// Channels referenced via (w17.event).channel below must
// be declared in the domain root's w17.proto cascade —
// proto/domains/{{.Domain}}/w17.proto under
// option (w17.module).channels. The "domain add
// --with-example" flow seeds both halves at once; bare
// "domain add" skips the channels block. If protoc / the
// codegen complains about undeclared channels, add a
// channels[] entry per the {{.Domain}} root file.
//
// Emit annotations live on the mutation RPCs — each
// (w17.event_emit) wires a write action to its signal;
// see mutations/note_mutation.proto.

package {{.Project}}.{{.Domain}}.{{.Module}};

import "google/protobuf/timestamp.proto";
import "w17/event.proto";

// NoteCreated — emitted on CreateNote. Carries enough
// context for downstream handlers to act without a
// round-trip to fetch the row.
message NoteCreated {
  option (w17.event) = {
    channel: "notes"
    topic:   "note.created"
  };

  int64                     note_id    = 1;
  int64                     owner_id   = 2;
  string                    title      = 3;
  google.protobuf.Timestamp created_at = 4;
}

// NoteCommentAdded — high-importance lifecycle signal
// with retry override. The default 5-deliveries / 30s
// ack is overridden upward (10 / 60s) because downstream
// uses (notification fan-out, mention detection,
// activity feed) cannot silently drop on transient
// broker errors.
message NoteCommentAdded {
  option (w17.event) = {
    channel: "notes"
    topic:   "note.comment_added"
    retry:   {
      max_deliver: 10
      ack_wait_seconds: 60
      backoff: BACKOFF_EXPONENTIAL
    }
  };

  int64                     note_id    = 1;
  int64                     author_id  = 2;
  google.protobuf.Timestamp added_at   = 3;
}

// NoteViewed — telemetry-shaped fire-and-forget. Best-
// effort delivery via the in-process MEMORY channel;
// drops on buffer-full are expected (analytics data,
// not business state). One delivery attempt; no retry.
message NoteViewed {
  option (w17.event) = {
    channel: "telemetry"
    topic:   "telemetry.note_viewed"
  };

  int64                     note_id   = 1;
  int64                     viewer_id = 2;
  google.protobuf.Timestamp viewed_at = 3;
}
`

// ExampleSubscribersProto — populated subscriber registry.
// Two subscriptions on the durable "notes" channel, both
// dispatched into the hand-written NotesBusiness facade.
// NoteViewed (telemetry/MEMORY channel) deliberately is NOT
// subscribed here — the standalone subscriber surface refuses
// MEMORY-channel events by design (single-process delivery,
// no cross-binary routing).
const ExampleSubscribersProto = `syntax = "proto3";

// PLACE THIS FILE AT THE DOMAIN ROOT:
//
//   proto/domains/{{.Domain}}/subscribers.proto
//
// NOT under a module ({{.Domain}}/{{.Module}}/...). Subscriber
// binaries are lifted per DOMAIN, so the sentinel lives at the
// domain root and a sub-module copy is rejected at codegen. One
// domain-root file carries the subscriptions for every module in
// the domain; add more surfaces as subscribers_<name>.proto
// beside it.
//
// The package below stays module-qualified even though the file
// sits at the domain root — it matches the handler refs, which
// are module-scoped ({{.Module}}.Service.Method).

package {{.Project}}.{{.Domain}}.{{.Module}};

import "w17/event_subscribers.proto";

// Subscriber registry for the example {{.Module}} module.
// Each entry pairs (event message, channel) with the
// hand-written handler that consumes it. The eventbus
// codegen folds every subscribers*.proto in this domain
// into one dispatch binary at cmd/{{.Domain}}-subscribers/.
option (w17.event_subscribers) = {
  subscriptions: [
    // Counter maintenance + activity-feed projection.
    // The handler is hand-written in
    // srcgo/domains/{{.Domain}}/grpcapi/business/.
    {
      event:   "NoteCreated"
      channel: "notes"
      handler: "{{.Module}}.NotesBusiness.OnNoteCreated"
    },
    // Notification fan-out — every comment triggers
    // downstream alerts via the same business facade.
    {
      event:   "NoteCommentAdded"
      channel: "notes"
      handler: "{{.Module}}.NotesBusiness.OnNoteCommentAdded"
    }
  ]
};
`

// ExampleDomainAdminProto — domain-level admin bundle
// declaration. One page per resource; storage codegen emits
// a separate admin binary (`<output>/<domain>-admin/`)
// alongside storage + gateway. The bundle reuses the
// existing query/mutation RPCs as data sources, so the
// admin surface comes essentially "for free" once the
// underlying CRUD exists.
const ExampleDomainAdminProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}};

import "w17/admin.proto";

// REV-150 — admin surface for the {{.Domain}} domain. Each
// page declares one resource view (list + detail) backed by
// query / mutation RPCs from the example module. Storage
// codegen emits a separate admin bundle alongside storage +
// gateway.
option (w17.admin_api) = {
  name:   "admin",
  prefix: "/admin",

  pages: [
    {
      name:  "NoteAdmin"
      model: "{{.Module}}.Note"
      list: {
        source:             "{{.Module}}.NoteQuery.ListNotes"
        columns:            ["id", "owner_id", "title", "created_at"]
        detail_link_column: "title"
      }
      detail: {
        read:            "{{.Module}}.NoteQuery.GetNote"
        update:          "{{.Module}}.NoteMutation.UpdateNote"
        delete:          "{{.Module}}.NoteMutation.DeleteNote"
        readonly_fields: ["id", "owner_id", "created_at"]
      }
    }
  ]
};
`

const ExampleBusinessProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}}.{{.Module}};

import "domains/{{.Domain}}/{{.Module}}/types/models.proto";

// NotesService — hand-written facade. Business-layer
// services have NO (w17.db.method) anywhere — every method
// body is implemented by hand in
// srcgo/domains/{{.Domain}}/grpcapi/business/. Storage codegen
// skips this file; the facade composes the auto-generated
// NoteQuery / NoteMutation calls + applies business rules.
//
// This stub exposes one search endpoint that fans out across
// queries (and would call validators / rate limiters / audit
// log mutations in a real implementation).
message SearchNotesReq  { string q = 1; }
message SearchNotesResp { repeated Note notes = 1; }

service NotesService {
  rpc SearchNotes(SearchNotesReq) returns (SearchNotesResp);
}
`

// ---------------------------------------------------------------------------
// Transport sentinels — gRPC-gateway (rpc), MCP, and CLI surfaces. Unlike
// the four-layer files above these are NOT laid down by `--with-example`
// today; `w17ctl template <rpc|mcp|cli>` is the only place they surface.
// Each is a domain-level zero-message sentinel mirroring rest.proto /
// admin.proto, modelled on the same fictional Note module so the refs
// line up with the rest of the example set.
// ---------------------------------------------------------------------------

// ExampleRpcProto — domain gRPC-gateway (rpc) surface sentinel
// (REV-152). Zero-message sentinel like rest/admin; unlike rest's
// central registry the method PICKS are inline (`(w17.rpc) = true` on
// each exposed rpc), so this file carries only surface metadata.
const ExampleRpcProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}};

import "w17/rpc.proto";

// REV-152 — public gRPC gateway surface for the {{.Domain}} domain.
// The generator re-declares the picked methods in a public
// schema/grpcapi.proto (reusing backend message types verbatim;
// streaming carries over) and emits a forward gateway. The methods
// are picked INLINE in their service files, NOT listed here:
//
//     rpc GetNote(GetNoteReq) returns (Note) {
//       option (w17.rpc) = true;                  // expose over the gateway
//       // option (w17.rpc_service) = "Notes";    // optional: regroup under a public service
//       option (w17.db.method) = { ops: { name: "main", dql: "..." } };
//     }
//
// File is a SENTINEL — options only, no messages.
option (w17.rpc_api) = {
  name:        "public",
  version:     "v1",
  description: "Example public gRPC surface for the {{.Domain}} domain.",
  // Default OFF, matching w17/rpc.proto's own policy — reflection is an
  // explicit opt-in there precisely so an internal-only surface keeps its
  // shape unadvertised, and a scaffold that hands every new project the
  // opposite quietly makes that policy untrue for every project anyone
  // actually creates. Turn it on while debugging with grpcurl; generated
  // stubs never needed it (w17ctl reads compiled contracts, not reflection).
  reflection:  false
};
`

// ExampleMcpProto — domain MCP surface sentinel (REV-018). Ref-based
// like rest: each tool references an existing gRPC method by
// `<module>.<Service>.<Method>`. One (w17.mcp_api) → one generated
// <domain>-mcp-server proxy bundle.
const ExampleMcpProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}};

import "w17/mcp.proto";

// REV-018 — MCP tool surface for the {{.Domain}} domain. The LLM
// reaches these tools during a session; each references an existing
// gRPC method (same ref shape as rest + subscribers). Streaming RPCs
// are a parse-time error (MCP has no streaming primitive). Tool name +
// description default from the method name + its leading comment;
// the *_override fields are the escape hatch when an LLM wants prose
// different from the human-facing comment.
//
// File is a SENTINEL — options only, no messages.
option (w17.mcp_api) = {
  name:        "default",
  version:     "v1",
  description: "Example MCP tools for the {{.Domain}} domain.",

  tools: [
    { ref: "{{.Module}}.NoteQuery.GetNote" },
    {
      ref:                  "{{.Module}}.NoteQuery.ListNotes"
      name_override:        "list_notes"
      description_override: "List notes, optionally filtered by owner."
    }
  ]
};
`

// ExampleCliProto — domain CLI surface sentinel (REV-153). Like rpc, a
// zero-message sentinel with inline per-method picks; unlike rpc each
// pick carries a command PATH. Emits the in-bundle <domain>-cli, plus a
// standalone <domain>-ctl for the methods that are also `(w17.rpc)`.
const ExampleCliProto = `syntax = "proto3";

package {{.Project}}.{{.Domain}};

import "w17/cli.proto";

// REV-153 — CLI surface for the {{.Domain}} domain. Generates a
// command-line tool with w17ctl-style namespace nesting. Methods are
// picked INLINE with a command path, NOT listed here:
//
//     rpc GetNote(GetNoteReq) returns (Note) {
//       option (w17.cli) = { command: "note get <note_id>" };
//       // <note_id> binds positionally; other request fields become --flags.
//       // Add option (w17.rpc) = true to also ship it in the standalone -ctl.
//       option (w17.db.method) = { ops: { name: "main", dql: "..." } };
//     }
//
// File is a SENTINEL — options only, no messages.
option (w17.cli_api) = {
  name:        "default",
  version:     "v1",
  description: "Example CLI for the {{.Domain}} domain."
};
`

// WriteExampleModule lays down the four-layer example payload under
// modDir. Shared by `domain add --with-example` (modDir =
// `<domain>/example/`) and `module add --with-example` (modDir =
// `<domain>/<module>/`). ctx.Module is required — every example
// template references it for the package suffix + the imports.
func WriteExampleModule(modDir string, ctx Ctx) ([]string, error) {
	if ctx.Module == "" {
		return nil, fmt.Errorf("WriteExampleModule: empty Module in scaffold ctx")
	}
	files := []struct {
		rel, name, body string
	}{
		{"w17.proto", "module_skeleton", ModuleSkeletonProto},
		{"types/models.proto", "example_types", ExampleTypesProto},
		{"queries/note_query.proto", "example_query", ExampleQueryProto},
		{"mutations/note_mutation.proto", "example_mutation", ExampleMutationProto},
		{"business/notes_business.proto", "example_business", ExampleBusinessProto},
		{"events/events.proto", "example_events", ExampleEventsProto},
		// NB: no subscribers.proto. The subscriber surface needs a
		// coherent event→handler business service (rpc OnX(Event) returns
		// (HandlerAck)) plus hand-written handler bodies — beyond a
		// minimal, codegen-clean example. events.proto above declares the
		// events (valid with no subscribers); the consumer shape is shown
		// by `w17ctl template subscribers`.
	}
	var written []string
	for _, f := range files {
		body, err := RenderTemplate(f.name, f.body, ctx)
		if err != nil {
			return written, err
		}
		dst := filepath.Join(modDir, f.rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return written, fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return written, fmt.Errorf("write %s: %w", dst, err)
		}
		written = append(written, RelFromProject(dst))
	}
	return written, nil
}

// RelFromProject formats an absolute path for log output by trimming it
// at the project root (the dir containing `w17/`). Falls back to the
// full path when no marker is found.
func RelFromProject(abs string) string {
	cur := abs
	for i := 0; i < 32; i++ {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		if _, err := os.Stat(filepath.Join(parent, "w17")); err == nil {
			rel, err := filepath.Rel(parent, abs)
			if err == nil {
				return rel
			}
		}
		cur = parent
	}
	return abs
}
