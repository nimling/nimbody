# Changelog

## Unreleased

1. `state`: export and import of a service's data through `GET /state`, `POST /state/export` and `POST /state/import`: a schema document with field flags, NDJSON state lines with a manifest and a hash per group, export by filter and key, upsert, create and sync imports that map keys to ids and converge on a second run, deletes by key, dry runs, per record results over the step stream, and stored snapshots.
2. `cli`: `StateCommand` adds `state`, `state export` and `state import` to a cli and its mcp tools.
3. `state`: field flags are JSON pointers, `/config/client_secret`, found through nested `properties` and through `oneOf`/`anyOf` with a `discriminator`; `fields.sensitive`, `immutable` and `server_default` in `GET /state` now hold pointers where they held top level names. Export strips exactly the flagged pointers and never the object around them; immutable and server default checks on import work by pointer.
4. `state`: `Group.Discriminator` and `Group.Sensitive` declare the secrets of each type; `GET /state` publishes them as `fields.discriminator` and `fields.sensitive_by`, and export strips each record by its own type as well as by the schema flags.
5. `terraform`: every cli that registers it is its own Terraform provider. Started by Terraform it serves each state group as `<provider>_<group>`, with read by key, create and update as an upsert import, delete by key and a dry run in the plan; secrets are write only and sensitive per pointer and per type, immutable fields force replace, server defaults are optional and computed, and references are keys. `<cli> terraform install` places the binary in the local plugin directory or a filesystem mirror, at the cli's version.
6. `cli`: `Provider` and `EnvTerraform`, the hook `Execute` serves the provider through when `TF_PLUGIN_MAGIC_COOKIE` is set; `terraform` joins the tooling commands that are not mcp tools.
7. Module: `hashicorp/terraform-plugin-go` v0.30.0 and what it brings, grpc and protobuf among them, imported only by `terraform/`; `stretchr/testify` to v1.10.0 and `golang.org/x/net`, `crypto` and `text` raised by it.

## v1.0.0

The first public release.

1. `nimbody`: request body binding that records which keys were sent, for partial updates, with `HasKey`, `HasValue`, `ForObject`, `Deny`, `IsEmpty` and `GetUnknown`.
2. `query`: validation of query parameters, pagination and cursors.
3. `respond`: one response envelope with data, error, pagination and request id, and a step stream over server sent events that ends in a result frame.
4. `cli`: the shared command line layer: exit codes, output as table, plain text or json (json when piped), output templates, named configs with a local override, env files and a write guard, and install commands for a Claude Code plugin and skill.
5. `mcp`: every command served as an mcp tool over stdio and streamable http, with tool annotations, inline images and an MCP App page.
6. `stream`: a loopback login page for clis and the client for the step stream.
