---
title: Packages
description: What the stream, mcp, cli, state and terraform packages of nimbody do for a service and for a company cli.
visibility: hidden
tags: nimbody, packages, stream, mcp, cli, config, output, state, export, import, terraform, provider
lastUpdated: true
---

## stream

The client side of the step stream. `EnvMCP` is the variable an mcp tool run sets on the cli it runs. `GetLoginPage` renders the page a cli's login loopback answers with, from an embedded page on the Mar tokens, with the product's name and mark from `LoginPage.Product`.

## mcp

The stdio model context protocol server every cli serves through, with an embedded run page and an embedded frame page.

1. A `Page` is an MCP App resource `ui://<cli>/<name>` with its CSP domains and its app only tools. A `Tool` whose `Page` names it links its result there instead of the run page.

2. Over streamable HTTP, `Define` is the one operation definition of a service, `Route` its echo routes, and `Server.Mount` mounts its tools at `/mcp` beside `/api`, behind the same auth middleware, with the RFC 9728 protected resource metadata and the 401 `WWW-Authenticate` challenge. A tool runs the service function the route runs, on the request's own context.

## cli

The shared cli layer the `cli` skill describes.

1. `Execute` runs with the family exit codes and `Error`. `Print` and `FormatFor` print a table on a terminal, json in a pipe and under mcp, and `FormatPlain` tab separated lines on request.

2. `Tools`, `Access` and `MCPCommand` generate annotated mcp tools from the cobra tree. A command annotated `AnnotationMCPPage` links its tool to a page the cli passes to `MCPCommand`.

3. `SkillCommand` serves the skill, and `SkillTreeCommand` a cli with references or several skills. `PluginCommand` has `install` for Claude Code, `desktop` for Claude Desktop and `status` for drift.

4. `Settings` is the config. `Flags` adds `--config`, `--env`, `--global`, `--yes` and `--plan`. `Load` layers the flag, the environment, `--env` files, the local `.<cli>/config.yaml` found walking up and the named config in `~/.<cli>/config.yaml` into the environment; the local config takes over from the named one. `Guard` asks before a write on a `protected` config. `ConfigCommand` is `config get`, `list`, `set`, `use`, `init`, `local` and `path`.

5. `Output` is the shared renderer with `--json`, `--raw`, `--plain`, `--fields`, `--sort`, `--jq`, `--template`, `--limit` and `--cursor`. `OperationCommand` builds a command from an `mcp.Operation`. `Bridge` and `Relay` make `<cli> mcp` relay stdio to the backend `/mcp` with the stored login.

## state

One path for a service to export and import its data, so a dataset is seeded, dumped, snapshotted, moved and restored through the api, the cli and mcp. nimbody owns the routing, the file format, the ordering, the key mapping, the dry run, the hashes and the step stream; the service owns its data and its access.

1. `Register` mounts `GET /state`, `POST /state/export` and `POST /state/import` behind the service's access middleware, takes one `Group` per kind of record and answers the three `mcp.Operation`s, `state`, `state_export` and `state_import`, for the service's `/mcp` server. A group names its create body schema from the OpenAPI spec, the groups it depends on, its reference fields with the group each points at, its accepted filters, and the `Export`, `Import`, `Delete` and `Blobs` callbacks that call the same service functions as its routes. Groups are ordered by their dependencies; a cycle or an unknown dependency stops the service at start.

2. `Context` carries the host's hooks: the access middleware, a transaction that wraps each record write so a refused record rolls back alone, the key `Resolver` that maps a key to an id and back, the asset store for snapshots, a progress callback per result, and the request's step reporter.

3. `GET /state` is the schema document: the format version, the service and its version, and every group in dependency order with its schema, `depends_on`, `refs`, `filters` and field flags. A property marked `writeOnly` or `x-sensitive` is a secret and is never exported; `x-immutable` cannot change in place; `x-server-default` is filled by the service, so an omitted value is not drift. Each flag is a JSON pointer, `/config/client_secret`, found by walking nested `properties`, and the branches of a `oneOf` or `anyOf` that carries an OpenAPI `discriminator`; a flag inside a branch holds for every record. Export removes exactly the flagged pointers and keeps the object around them, and the import compares immutable and server default fields by pointer.

3.1. A secret that only one type of record carries is declared per type, never as a union: `Group.Discriminator` is the pointer that names the type, `/type`, and `Group.Sensitive` maps each of its values to that type's secret pointers. `GET /state` publishes them as `fields.discriminator` and `fields.sensitive_by`, and export strips each record by its own type as well as by the schema flags. A secret sent on import always counts as a change, so it is written.

4. `POST /state/export` takes `targets`, each a group with a `filter` and `keys`, and `history`, `blobs` and `store`. No targets means every group the caller reaches. It answers `application/x-ndjson`: a manifest line with the format, service, version, scope and per group count and sha256 hash, then one line per record `{group, key, data, refs}` and one per blob `{blob, group, key, sha256, mime, size, data}`. A record's key is its external id or its source id, and a reference is written as the key of the record it points at. With `Accept: text/event-stream` the lines arrive as the data of the result frame after one step per group; with `store` the export is saved as an asset and the answer is its `asset_id` and manifest.

5. `POST /state/import` takes the lines as `application/x-ndjson`, with `mode`, `dry_run`, `delete=group:key` and `target=group:field=value` in the query, or a json body with `asset_id` and the same options. `mode` is `upsert` by default, `create`, or `sync`, which also deletes every record the targets reach that the input lacks, or the scope the export was taken with when no target is given. Keys map to ids through the resolver and a created record keeps its key as its external id, so a second run reports every record `unchanged` and writes nothing. Every record answers `{key, group, id, outcome, error}`, with `outcome` one of `created`, `updated`, `unchanged`, `deleted` or `refused`, in the response and as one step per record. A dry run answers the same outcomes, deletes included, and writes nothing.

6. `cli.StateCommand` gives every company cli `state`, `state export [--target group[:filter]]… [--key group:key]… [--store] [--history] [--blobs] -o file` and `state import [file] [--dry-run] [--mode upsert|create|sync] [--delete group:key]… [--target …] [--asset id]` over the cli's own `Send`, and through the cli's mcp server the same three tools with progress.

## terraform

Every company cli is its own Terraform provider, the same way it is its own mcp server, and the service's `GET /state` is its schema. The package is the only one that imports `hashicorp/terraform-plugin-go`, so a cli or service that does not import it builds none of it.

1. `terraform.Register(root, terraform.Provider{Name, Version})` is the one call a cli makes. It adds `terraform install` and sets `cli.Provider`, so `cli.Execute` serves the provider over plugin protocol 6 when Terraform starts the binary with `TF_PLUGIN_MAGIC_COOKIE`, and runs the cli as always otherwise. `Source` is the provider address, `nimling/<name>` on `registry.terraform.io` by default; its type is the cli's name.

2. The provider reaches the service only through the cli itself, as the stdio mcp tools do: it runs the same binary with `state`, `state export --key group:key` and `state import -`, so the login, the config, the environment and a protected config's refusal are the cli's own. The provider block takes `config`, the named config to run with as `--config`.

3. Each group of the schema document is a resource `<provider>_<group>`. `key` is required and is the record's identity; a change of key replaces the resource. Every property of the group's schema is an attribute, nested objects as nested attributes, lists of objects as nested lists, and the branches of a discriminated `oneOf` or `anyOf` merged into one object. A reference field holds the key of the record it points at, so a configuration writes `location_id = demo_location.hq.key`.

4. The field flags map per pointer. A secret, from `fields.sensitive` and every type in `fields.sensitive_by`, is write only and sensitive at its own nested attribute: it is sent from the configuration on every create and update and never enters the plan or the state. A pointer the schema lacks becomes an optional write only string. An immutable field forces replace when it changes. A server default is optional and computed: unknown until the first apply, then kept from the state while the configuration leaves it out.

5. Read exports the record by key, and a record that is gone leaves the state. Create and update are an upsert import of the one record, read back afterwards to fill what the service set. Delete is an import with `delete`, and a record already gone is not an error. The plan runs the import as a dry run and turns a refusal into a warning, since a reference to a record created later in the same apply is not there yet. `terraform import` takes the key.

6. The provider reads only the state format it was built with and stops with an error naming both when the service speaks another. Its version is the cli's version: `<cli> terraform install` copies the binary to `~/.terraform.d/plugins/<host>/<namespace>/<name>/<version>/<os>_<arch>/terraform-provider-<name>_v<version>`, or under `--dir` for a filesystem mirror, and refuses a version that is not a release.
