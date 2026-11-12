---
title: Packages
description: What the stream, mcp, cli, state, terraform and endpoint packages of nimbody do for a service and for a company cli.
visibility: hidden
tags: nimbody, packages, stream, mcp, cli, config, output, state, export, import, terraform, provider, endpoint
lastUpdated: true
---

## stream

The client side of the step stream. `EnvMCP` is the variable an mcp tool run sets on the cli it runs. `GetLoginPage` renders the page a cli's login loopback answers with, from an embedded page on the Mar tokens, with the product's name and mark from `LoginPage.Product`.

## mcp

The stdio model context protocol server every cli serves through, with an embedded run page and an embedded frame page.

1. A `Page` is an MCP App resource `ui://<cli>/<name>` with its CSP domains and its app only tools. A `Tool` whose `Page` names it links its result there instead of the run page.

2. Over streamable HTTP, `Define` is the one operation definition of a service, `Route` its echo routes, and `Server.Mount` mounts its tools at `/mcp` beside `/api`, behind the same auth middleware, with the RFC 9728 protected resource metadata and the 401 `WWW-Authenticate` challenge. A tool runs the service function the route runs, on the request's own context. `Spec.Example` is the operation's example, on its tool's description and its cli command.

## cli

The shared cli layer the `cli` skill describes.

1. `Execute` runs with the family exit codes and `Error`. `Print` and `FormatFor` print a table on a terminal, json in a pipe and under mcp, and `FormatPlain` tab separated lines on request.

2. `Tools`, `Access` and `MCPCommand` generate annotated mcp tools from the cobra tree. A command annotated `AnnotationMCPPage` links its tool to a page the cli passes to `MCPCommand`.

2.1. A tree annotated `AnnotationMCPInput: InputTyped` gets typed tool input: one property per flag and per positional of the usage line, an `id` positional named for its parent (`article_id`), the required ones listed. Without it a tool takes `{args, flags}`, and a typed tool accepts that shape too. A secret setting's flag, or a flag annotated `AnnotationSecret`, is never an input. The description is the command path, `Short`, the usage line and the `Example`, at most 4096 characters.

2.2. `PutOutput[Out]` declares a command's output struct: its tool carries `outputSchema` `{data: …}` and answers `structuredContent` from the json the command prints, unless the call shapes it. A destructive tool carries `_meta["anthropic/requiresUserInteraction"]`. The server answers `GetInstructions(root, name)` in `initialize`, the root's `AnnotationMCPInstructions` or a few lines from its `Short`.

3. `SkillCommand` serves the skill, and `SkillTreeCommand` a cli with references or several skills. `PluginCommand` has `install` for Claude Code, `desktop` for Claude Desktop and `status` for drift.

4. `Settings` is the config. `Flags` adds `--config`, `--env`, `--global`, `--yes` and `--plan`. `Load` layers the flag, the environment, `--env` files, the local `.<cli>/config.yaml` found walking up and the named config in `~/.<cli>/config.yaml` into the environment; the local config takes over from the named one. `Guard` asks before a write on a `protected` config. `ConfigCommand` is `config get`, `list`, `set`, `use`, `init`, `local` and `path`; `--plan` prints the write `set`, `use`, `init` or `local` would make and stops. A secret is never a flag: `Root` refuses a `Setting` that is `Secret` and names a `Flag`.

4.1. `Settings` holds the credentials too. `GetCredential`, `PutCredential`, `DeleteCredential`, `GetCredentials` and `LockCredential` keep a login per host in `~/.<cli>/<host>.cred` and a machine token per host and client in `<host>_<client>.client`, the tokens in the OS keychain (`Keychain`: `security` on macOS, `secret-tool` on Linux) and only where none answers in the 0600 file. `Get` answers a key's resolved value.

4.2. `Client` is the api client and its login. The credential is `<PREFIX>TOKEN` (or `<PREFIX>ACCESS_TOKEN`) for CI, else the stored login, refreshed before expiry under the cross process lock, else the machine pair sent as Basic auth to the service. The cli names its own `LoginPath`, `TokenPath`, `RevokePath`, `IdentityPath` and `AppHeader`, which `Mount` requires; nimbody holds no default for them. A cli whose service issues tokens sets `Exchange` and exchanges the pair for a token, cached under the same lock, so parallel runs share one. `Send` sends through it with the step stream, the `User-Agent` and the family exit codes. `Mount` adds `login` (PKCE, loopback or `--no-browser`), `logout`, `status` and `token`, and makes `Remote` the default `Bridge`: `<cli> mcp` relays to the service's `/mcp` when `/status` lists it and the credential is a bearer token, and serves the tools locally otherwise.

5. `Output` is the shared renderer with `--json`, `--raw`, `--plain`, `--fields`, `--sort`, `--jq`, `--template`, `--limit` and `--cursor`, and on a `Root` also `--format concise|detailed` and `--query key=value`. `OperationCommand` builds a command from an `mcp.Operation`. `Table` builds the resource commands from `Resource` and `Verb` declarations, with the ids positional in path order, the access from the method, the body flags and an `Example` per command; `Body` is `--set`, `--data`, `--file` and `--upload`, and `APICommand` is `api <method> <path>`. `Bridge` and `Relay` make `<cli> mcp` relay stdio to the backend `/mcp` with the stored login.

6. `Root(root, settings, output)` is how a cli starts: it mounts `Settings` and `Output`, typed mcp input, and loads and guards before the cli's own `PersistentPreRunE`. `HelpCommand` is `help`, the bundled SKILL.md on stdout, and `HelpFunc` puts the banner above the root's help; both show the banner on a terminal only. `config get` and `config list` honour `--json`.

7. `CompletionCommand` is `completion`, with `--auto` installing under the XDG data dir and one managed rc block. `Version` is the one ldflags symbol every cli sets, `GetVersion` its value, and `VersionCommand` prints it with the server's version, `Client.GetServerVersion` from `/status`. `Notifier` notes a newer version on stderr after a command, once a day at most, on a terminal only, off with `<PREFIX>NO_UPDATE_NOTIFIER`.

## state

One path for a service to export and import its data, so a dataset is seeded, dumped, snapshotted, moved and restored through the api, the cli and mcp. nimbody owns the routing, the file format, the ordering, the key mapping, the dry run, the hashes and the step stream; the service owns its data and its access.

1. State is an ordinary api endpoint under the service's `/api`, behind the same middleware as every other route and with no access path of its own. `Register(host, middleware, groups...)` takes the service's `/api` chain and mounts `GET /api/state`, `POST /api/state/export`, `POST /api/state/import` and the import session routes with it, takes one `Group` per kind of record and answers the `mcp.Operation`s `state`, `state_export`, `state_import`, `state_import_session`, `state_import_part`, `state_import_commit` and `state_import_session_delete` for the service's `/mcp` server. `GetDocument(service, version, groups...)` answers the schema document from the definitions alone, with no request. A group names its create body schema from the OpenAPI spec, the groups it depends on, its reference fields with the group each points at, its accepted filters, and the `Export`, `Import`, `Delete` and `Blobs` callbacks that call the same service functions as its routes. Groups are ordered by their dependencies; a cycle or an unknown dependency stops the service at start.

2. `Context` carries the host's hooks: a transaction that wraps each record write so a refused record rolls back alone, the key `Resolver` that maps a key to an id and back, the asset store for snapshots, a progress callback per result, the request's step reporter, and the headers every operation takes, such as the app slug.

3. `GET /api/state` is the schema document: the format version, the service and its version, and every group in dependency order with its schema, `depends_on`, `refs`, `filters` and field flags. A property marked `writeOnly` or `x-sensitive` is a secret and is never exported; `x-immutable` cannot change in place; `x-server-default` is filled by the service, so an omitted value is not drift. Each flag is a JSON pointer, `/config/client_secret`, found by walking nested `properties`, and the branches of a `oneOf` or `anyOf` that carries an OpenAPI `discriminator`; a flag inside a branch holds for every record. Export removes exactly the flagged pointers and keeps the object around them, and the import compares immutable and server default fields by pointer.

3.1. A secret that only one type of record carries is declared per type, never as a union: `Group.Discriminator` is the pointer that names the type, `/type`, and `Group.Sensitive` maps each of its values to that type's secret pointers. `GET /api/state` publishes them as `fields.discriminator` and `fields.sensitive_by`, and export strips each record by its own type as well as by the schema flags. A secret sent on import always counts as a change, so it is written.

4. `POST /api/state/export` takes `targets`, each a group with a `filter` and `keys`, and `history`, `blobs` and `store`. No targets means every group the caller reaches. It answers `application/x-ndjson`: a manifest line with the format, service, version, scope and per group count and sha256 hash, then one line per record `{group, key, id, data, refs}` and one per blob `{blob, group, key, sha256, mime, size, data}`. A record's key is its external id or its source id, and a reference is written as the key of the record it points at. With `Accept: text/event-stream` the lines arrive as the data of the result frame after one step per group; with `store` the export is saved as an asset and the answer is its `asset_id` and manifest.

5. `POST /api/state/import` takes the lines as `application/x-ndjson`, with `mode`, `dry_run`, `delete=group:key` and `target=group:field=value` in the query, or a json body with `asset_id` and the same options. `mode` is one of five. `upsert`, the default, creates what is missing and updates what differs. `create` refuses a record that exists and differs. `sync` also deletes every record the targets reach that the input lacks, or the scope the export was taken with when no target is given. `skip` leaves every record that exists as it is and reports it `unchanged`. `replace` deletes every record of every group the input lacks before it imports. Deletes run before the imports, in reverse dependency order, so nothing is refused on a removed record. Keys map to ids through the resolver and a created record keeps its key as its external id and takes the `id` of its line when that id is free, so a second run reports every record `unchanged` and writes nothing. Every record answers `{key, group, id, outcome, error}`, with `outcome` one of `created`, `updated`, `unchanged`, `deleted` or `refused`, in the response and as one step per record. A dry run answers the same outcomes, deletes included, and writes nothing.

5.1. A file too large for one request goes through an import session. `POST /api/state/import/session` opens one and answers its id, `PUT /api/state/import/session/:id/part/:part` stages one numbered part of whole state lines, numbered from 1 and in any order, `POST /api/state/import/session/:id/commit` imports every staged part as one file with the same options as the import, and `DELETE /api/state/import/session/:id` drops it. A session holds at most 512 MiB, lives one hour, and a dry run commit keeps it.

6. `cli.StateCommand` gives every company cli `state`, `state export [--target group[:filter]]… [--key group:key]… [--store] [--history] [--blobs] -o file` and `state import [file] [--dry-run] [--mode upsert|create|sync|skip|replace] [--delete group:key]… [--target …] [--asset id]` over the cli's own `Send` to `/api/state`, and through the cli's mcp server the same tools with progress.

## terraform

Every company cli is its own Terraform provider, the same way it is its own mcp server, a plain client of the service's `/api/state` like any public provider. Its schema is the state document the binary is built with. The package is the only one that imports `hashicorp/terraform-plugin-go`, so a cli or service that does not import it builds none of it, and nothing of Terraform runs in a service: no registry, no state backend, no release token and no signing key.

1. `terraform.Register(root, terraform.Provider{Name, Version, Client, Document})` is the one call a cli makes. It adds `terraform install`, `terraform package` and `terraform schema` and sets `cli.Provider`, so `cli.Execute` serves the provider over plugin protocol 6 when Terraform starts the binary with `TF_PLUGIN_MAGIC_COOKIE`, and runs the cli as always otherwise. `Client` is the cli's own client and `Document` the state document from code, `state.GetDocument` over the service's definitions or a schema generated at build and embedded. `Source` is the provider address, `nimling/<name>` on `registry.terraform.io` when left empty, or `<host>/<namespace>/<name>` for a filesystem mirror; its type is the cli's name.

2. The provider block takes `endpoint`, `client_id`, `client_secret`, `token` and `app_slug`, each else from the cli's environment, `<PREFIX>ENDPOINT` and the rest, with the cli's credential order: a token before the client pair. A provider without an endpoint answers an error naming the variable. Every call is a request to `/api/state` through the service's own middleware, as the cli makes it, and no secret sits in a `.tf` file.

3. Each group of the schema document is a resource `<provider>_<group>`. `key` is required and is the record's identity; a change of key replaces the resource. Every property of the group's schema is an attribute, nested objects as nested attributes, lists of objects as nested lists, and the branches of a discriminated `oneOf` or `anyOf` merged into one object. A reference field holds the key of the record it points at, so a configuration writes `location_id = demo_location.hq.key`.

4. The field flags map per pointer. A secret, from `fields.sensitive` and every type in `fields.sensitive_by`, is write only and sensitive at its own nested attribute: it is sent from the configuration on every create and update and never enters the plan or the state. A pointer the schema lacks becomes an optional write only string. An immutable field forces replace when it changes. A server default is optional and computed: unknown until the first apply, then kept from the state while the configuration leaves it out.

5. Read exports the record by key, and a record that is gone leaves the state. Create and update are an upsert import of the one record, read back afterwards to fill what the service set. Delete is an import with `delete`, and a record already gone is not an error. The plan runs the import as a dry run and turns a refusal into a warning, since a reference to a record created later in the same apply is not there yet. `terraform import` takes the key.

6. The provider reads only the state format it was built with and stops with an error naming both when the service speaks another. Its version is the cli's version: `<cli> terraform install` copies the binary to `~/.terraform.d/plugins/<host>/<namespace>/<name>/<version>/<os>_<arch>/terraform-provider-<name>_v<version>`, or under `--dir` for a filesystem mirror, and refuses a version that is not a release. `<cli> terraform schema [-o file]` writes the provider schema as `terraform providers schema -json` does, from the document in the binary, with no endpoint and no login, so docs and checks need no running service.

7. `<cli> terraform package --dir dist --modules deploy/terraform <os>_<arch>=<binary>…` writes the release files from the builds the release made: `terraform-provider-<name>_<version>_<os>_<arch>.zip` per platform holding `terraform-provider-<name>_v<version>`, `terraform-provider-<name>_<version>_manifest.json` with protocol `6.0`, `terraform-provider-<name>_<version>_SHA256SUMS` over the zips and the manifest, and `terraform-module-<module>-<system>_<version>.tar.gz` for each `deploy/terraform/<module>/<system>/`, without `.terraform/`, state or variable files. It prints each path and names the file to sign: the release signs the sums with the org key into `terraform-provider-<name>_<version>_SHA256SUMS.sig`, a binary detached signature, and attaches all of them to the GitHub release, the layout the public registry reads. `terraform.PutPackage` is the same step as a function, `terraform.GetFiles` and `terraform.GetModuleArchive` the file names.

8. A configuration sources a provider as `nimling/<cli>` from `registry.terraform.io`, at the cli's version. The registry reads one public repository per provider, `nimling/terraform-provider-<cli>`, and nothing in it is written by hand: when a service releases, nimbody's `terraform-provider.yml` workflow copies the provider files and the generated docs of that release into the provider repository, as its `docs/` and a release under the same tag.

## endpoint

The common endpoint set every api backed service mounts in one call, so the paths are the same on every host: `/api` stays the service's own, and `endpoint.Mount(e, endpoint.Set{…})` adds the rest.

1. `/status` answers the service, its version and the endpoints it serves, and `/status/health` 200, or 503 when the `Health` hook fails.

2. `State` with its `Groups` mounts `/api/state` through `state.Register`, behind `APIMiddleware`, the service's own `/api` chain, filling the service and version from the set, and hands the state operations to `MCP`.

3. `MCP`, when the service serves tools, mounts `/mcp` through `Server.Mount` behind `MCPAccess`.
