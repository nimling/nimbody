# Package rules

## Boundaries

1. `nimbody` (root): body binding and validation only. Depends on stdlib and echo.

2. `query/`: query parameter whitelisting, alias groups, with token parsing, typed accessors, time parsing. Depends on stdlib, echo, uuid.

3. `respond/`: response envelope, pagination and cursors, sort column allowlisting, scope filtering, request id middleware, JSON serializer. Depends on stdlib, echo, uuid, bun.

3.1. `mcp/`: the mcp server on stdio for a cli and over streamable HTTP for a service, its run page and the pages a cli registers, the reusable frame page, and `Define`, the one operation definition a route, its tool and its cli command share. Depends on stdlib, echo and `respond`, `stream`, `shape`.

3.2. `cli/`: the cli layer every company cli shares, its exit codes, output, colour, tool generation, skill command, config with its write guard, the `Output` renderer, `OperationCommand` and the stdio bridge to a backend `/mcp`; how the config layers: `.nimpress/packages.md`. Depends on stdlib, cobra, pflag, yaml.v3, godotenv and `mcp`, `shape`. A cli copies none of it.

3.3. `shape/`: output shaping the cli renderer and the remote mcp tools share, fields, sort, jq and templates. Depends on stdlib and itchyny/gojq.

4. The dependency whitelist for the whole module is stdlib, echo, google/uuid, uptrace/bun, and spf13/cobra with spf13/pflag, gopkg.in/yaml.v3 and joho/godotenv for `cli/` alone, itchyny/gojq for `shape/` alone, and stretchr/testify for `test/` alone. Nothing else enters go.mod without a coordinated decision.

5. Nothing app specific enters any package: no models, no ORM setup, no database connections beyond the bun select in `respond`, no samna auth primitives, no imports from consumer repos. godotenv is for `example/` and `cli/` only.

## Source of truth

1. This module owns the envelope and validation logic for every consumer `CLAUDE.md` names. A bug in respond or query is fixed here and released; it is never patched in a consumer's vendored or copied code, because no such copy is allowed to exist.

2. When a consumer needs new behavior, the behavior is added here behind the existing function surface, released, and then consumed by version bump.

## Compatibility

1. Additive exported surface: minor bump.

2. Behavior change behind an existing signature, changed sentinel error text that consumers match on, changed validation strictness: minor at best, major when a consumer would need code changes.

3. Any change to envelope JSON keys, the pagination block, cursor encoding, or reserved query parameter names: major bump, coordinated upgrade across all consumers in the same change set.

4. Before releasing, check the call sites in every consumer for every symbol the change touches.
