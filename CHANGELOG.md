# Changelog

## Unreleased

1. `state`: export and import of a service's data through `GET /state`, `POST /state/export` and `POST /state/import`: a schema document with field flags, NDJSON state lines with a manifest and a hash per group, export by filter and key, upsert, create and sync imports that map keys to ids and converge on a second run, deletes by key, dry runs, per record results over the step stream, and stored snapshots.
2. `cli`: `StateCommand` adds `state`, `state export` and `state import` to a cli and its mcp tools.
3. `state`: field flags are JSON pointers, `/config/client_secret`, found through nested `properties` and through `oneOf`/`anyOf` with a `discriminator`; `fields.sensitive`, `immutable` and `server_default` in `GET /state` now hold pointers where they held top level names. Export strips exactly the flagged pointers and never the object around them; immutable and server default checks on import work by pointer.
4. `state`: `Group.Discriminator` and `Group.Sensitive` declare the secrets of each type; `GET /state` publishes them as `fields.discriminator` and `fields.sensitive_by`, and export strips each record by its own type as well as by the schema flags.
5. `terraform`: every cli that registers it is its own Terraform provider. Started by Terraform it serves each state group as `<provider>_<group>`, with read by key, create and update as an upsert import, delete by key and a dry run in the plan; secrets are write only and sensitive per pointer and per type, immutable fields force replace, server defaults are optional and computed, and references are keys. `<cli> terraform install` places the binary in the local plugin directory or a filesystem mirror, at the cli's version.
6. `cli`: `Provider` and `EnvTerraform`, the hook `Execute` serves the provider through when `TF_PLUGIN_MAGIC_COOKIE` is set; `terraform` joins the tooling commands that are not mcp tools.
7. Module: `hashicorp/terraform-plugin-go` v0.30.0 and what it brings, grpc and protobuf among them, imported only by `terraform/`; `stretchr/testify` to v1.10.0 and `golang.org/x/net`, `crypto` and `text` raised by it.
8. `terraform/registry`: the running service is the Terraform registry of its own provider and modules. `Mount` serves `/.well-known/terraform.json`, the Provider Registry Protocol under `/terraform/providers/v1/samna/<cli>/` with versions, platforms, the download with its `SHA256SUMS`, signature and the org public key, and the Module Registry Protocol under `/terraform/modules/v1/samna/<module>/<system>/`, exactly at the version the instance runs. Files stream through the instance from the repo's GitHub release with the read token the host's `Release.Token` hook answers; a release that cannot be read answers 502.
9. `terraform/registry`: an optional http state backend at `/terraform/state/<workspace>` with `LOCK` and `UNLOCK`, 423 with the held lock on a conflict, behind the host's access middleware and over the host's storage callbacks.
10. `terraform`: `<cli> terraform package` writes the release files, a zip per platform, the `SHA256SUMS` to sign and a tar.gz per module in `deploy/terraform/<module>/<system>/`; the provider namespace defaults to `samna`.
11. `endpoint`: `Mount` with a `Set` mounts the common endpoint set, `/status`, `/status/health`, `/state`, `/mcp` and the Terraform registry, beside the service's own `/api`.
12. Module: `golang.org/x/crypto` is a direct requirement, for the test of the signed sums under `test/` only.
13. `cli`: `config set --config <name> key=value` creates a named config that does not exist yet, as the refusal's hint says; `Load` no longer refuses the unknown name for that one command, and every other command still refuses it with the same message and hint.
14. `respond`: a route declares the largest step stream frame it sends with `FrameLimitMiddleware(bytes)`, `0` for unbounded, and every stream announces it in the `X-Frame-Limit` header; a route that declares nothing announces `FrameLimit`, 16 MiB, as before. `stream.Read` sizes its line scanner from that header, so a result frame over 16 MiB no longer fails with `bufio.Scanner: token too long` where the route allows it. `POST /state/export` declares an unbounded frame, so a streamed export with blobs reads whole.
15. The step stream frame limit reaches the mcp path. `/mcp` announces `X-Frame-Limit` from the operation it calls, `stream.Scanner` is the one reader sized from that header, and the stdio bridge reads the service's `/mcp` stream through it, so a `state_export` result over 16 MiB passes `<cli> mcp`. The `state_export` tool declares the unbounded frame its route does. `mcp.Exec` reads the cli's own stderr without a fixed cap. The lines a host sends on stdin to `<cli> mcp` stay capped at 16 MiB, as no endpoint has answered when they are read.

## v1.0.0

The first public release.

1. `nimbody`: request body binding that records which keys were sent, for partial updates, with `HasKey`, `HasValue`, `ForObject`, `Deny`, `IsEmpty` and `GetUnknown`.
2. `query`: validation of query parameters, pagination and cursors.
3. `respond`: one response envelope with data, error, pagination and request id, and a step stream over server sent events that ends in a result frame.
4. `cli`: the shared command line layer: exit codes, output as table, plain text or json (json when piped), output templates, named configs with a local override, env files and a write guard, and install commands for a Claude Code plugin and skill.
5. `mcp`: every command served as an mcp tool over stdio and streamable http, with tool annotations, inline images and an MCP App page.
6. `stream`: a loopback login page for clis and the client for the step stream.
