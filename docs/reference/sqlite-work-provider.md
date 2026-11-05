---
title: Native SQLite Work Provider
description: Store city and rig work in local SQLite without an external database service.
---

Set `[beads] provider = "sqlite-work"` to keep work tickets in native SQLite,
without `bd`, a Dolt server, or an external provider process.

Each configured scope owns `.gc/store/work/beads.sqlite` and an `identity.json`
record of its provider and ID prefix. The city and every rig have separate work
stores. The ordinary opener refuses a missing database or mismatched identity.
Only explicit initialization creates a database. Work history has no automatic
retention sweep.

The existing infrastructure split is unchanged:

```toml
[beads]
provider = "sqlite-work"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store/graph"
```

Do not point `classes.work` at `infra`. The reserved `work` binding still routes
to the city or rig selected by the caller. Infrastructure keeps its own provider,
reserved ID namespace, convergence checks, and existing database.

## Initialization and cutover

Initialize a configured scope explicitly with `gc --rig <name> bd init`, or
`gc --city <path> bd init` for city work. Normal city startup initializes missing
native scopes as well. These paths do not start or stop an external service.

Initialization refuses legacy `.beads/metadata.json`, `.gc/beads.json`, and an
existing native database without its identity file. It does not import or delete
legacy work. An operator must separately authorize and complete an archival,
fresh reset, or data migration. Existing infrastructure migration commands move
infrastructure classes only and must not be described as work import commands.

Switching providers changes the work-store opener identity used by storage
convergence checks. Follow the existing storage status/migration protocol rather
than editing convergence markers. Do not change the binding location of an
existing infrastructure database as part of a work-provider cutover.

## Native command surface

The `gc bd` adapter handles these commands in process:

- `init`
- `create` / `new`, `list`, `show`
- `update`, `close`, `reopen`
- `heartbeat`, `reclaim --id`, `release-if-current ID ASSIGNEE`
- `dep add`, `dep remove`, `dep list`
- `comments add ID TEXT`, `comments list ID`

Common flags are `--json`, `--actor`, and the existing outer scope flags.
Creation accepts title, description, type, priority, assignee, ID, parent, labels,
notes, external reference and string-valued metadata. Updates additionally accept
`--append-notes`, label additions/removals and metadata removals. Notes, external
references and comments persist in work metadata. `show --json`
returns an array. `create --json` returns one object.

Unknown commands and unsupported flags fail instead of falling back to `bd`.
Use `gc ready` for whole-city readiness, including infrastructure workflow steps.
Generated worker queries use this reader for native work providers. `gc hook`
claim, release, recovery, continuation, metadata and workflow operations use the
native stores, while infrastructure claims retain their existing class routing.
A raw `bd` executable does not read these databases.

Native default `on_boot` and `on_death` recovery hooks also use `gc ready` and
`gc bd`, including relocated infrastructure rows. Their updates require the
observed owner and status to match atomically through `--if-assignee` and
`--if-status`. A mismatch leaves the row unchanged. Explicit custom hooks and
queries remain operator-owned shell commands and must be migrated separately.

Dependencies added by this adapter must reference beads in the same work store.
Cross-rig prerequisites require explicit coordinator gates or the existing
cross-store workflow routing. They must not be assumed to resolve by ID alone.

## Claim leases

`update ID --claim` atomically claims an open/unassigned ticket and records a
30-minute lease. `--actor` defaults to `BEADS_ACTOR`. `heartbeat ID` renews the
lease only for the current owner while the ticket remains in progress. Hook
claims use the same atomic operation and their explicit session identity.

`reclaim --id ID` releases only an expired native lease. A missing or malformed
lease does not authorize taking work from another owner. Release and adoption
operations compare the current owner before changing it, and ownership changes
advance the existing claim fence. This provider does not infer worker death from
a timeout or perform automatic reclaims without the caller's existing policy.

## Validation boundary

The provider tests use real temporary SQLite databases and independent open
handles. They exercise claim exclusion, heartbeat ownership, expiry recovery,
close/reopen persistence, CLI notes/comments, hook claims, dependency readiness
and scope identity refusal. Production acceptance still requires a disposable
city with the intended infrastructure split and agent configuration before a
live cutover.
