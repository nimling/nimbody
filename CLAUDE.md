# nimbody

The shared contract of a Go service and its cli. Every Go service imports its http handler packages: samna_auth, samna_auth_middleware, bookable_server, samna_sync, samna_connect, hvergel, samna_sign, samna-developer, nimployer and kjetployer. Every company cli imports `cli/`, `mcp/` and `stream/`: sauth, book, hvrg, scon, ssync, nimp, smig, sbump, samna and nimon; sdm imports `mcp/` and `stream/` without `cli/`. A change to behaviour, a signature, a sentinel error text or a JSON key breaks all of them at once. GitHub `nimling/nimbody`.

## Commands

1. `just test [TestName]`: `go test ./test/...`, with `-run` when a name is given.
2. `just build`: `go build ./...`.
3. `just tidy`: `go mod tidy`.
4. `just deploy`: `sbump patch --env APP_VERSION --push-version --auto --workflow`; `.github/workflows/release.yml` makes the GitHub release on the tag.
5. `just install`: checks go against `go.mod` and sbump, installs what is missing or older, then the module dependencies.

## Packages

What `stream/`, `mcp/`, `cli/` and `state/` do: `.nimpress/packages.md`.

1. Root `main.go`: body binding and validation, `Bind`, `Validator`, `BodyValidator`, `Body`.
2. `query/`: query parameter whitelisting and typed access, `ValidateQueryParams`, `QueryParams`, `ParseWith`, `GetTimeParam`.
3. `respond/`: envelope, pagination and cursors, sort column allowlist, scope filtering, `RequestIDMiddleware`, `FriendlyJSONSerializer`, the step stream (`GetSteps`, `StepsFrom`, `Streaming`). What is frozen: `.claude/rules/envelope.md`; the contract: `.nimpress/envelope.md`.
4. `stream/`: the client side of the step stream, `Read`, `Report`, `Line`, `Parse`, `Machine`, `EnvMCP`, and `GetLoginPage` with `LoginPage.Product` from the embedded `login.html`.
5. `mcp/`: the stdio mcp server every cli serves through, `Server`, `Tool`, `Page`, `Command`, `Annotate`, `Argv`, `Exec`, `Frame`, the embedded `page.html` and `frame.html`; over streamable HTTP `Define`, `Route` and `Server.Mount`.
6. `cli/`: the shared cli layer the `cli` skill in `~/.claude/skills/cli` describes: `Execute`, `Error`, `Print`, `FormatFor`, `FormatPlain`, `Colour`, `Style`, `Tools`, `Access`, `MCPCommand`, `AnnotationMCPPage`, `SkillCommand`, `SkillTreeCommand`, `PluginCommand`, `Settings`, `Flags`, `Load`, `Guard`, `ConfigCommand`, `Output`, `OperationCommand`, `Bridge`, `Relay`, `StateCommand`, `Send`.
7. `shape/`: fields, sort, jq and templates over decoded json, shared by `cli.Output` and the remote mcp tools.
8. `state/`: a service's data out and in through `/api/state`, behind the service's own `/api` middleware. `Register(host, middleware, groups...)` with one `Group` per resource, `GetDocument` for the schema document, `Context` with the host's transaction, `Resolver` and asset hooks, field flags as JSON pointers, `Read` and `File` for the NDJSON lines, and the `state` commands every cli carries.
9. `terraform/`: each cli as its own Terraform provider, a plain client of `/api/state`. One resource per state group, `Register` with `Provider{Name, Source, Version, Client, Document}`, the commands `terraform install`, `package` and `schema`, and the release files `PutPackage`, `GetFiles` and `GetModuleArchive`. The only package that imports `hashicorp/terraform-plugin-go`. Nothing of it runs in a service.
10. `endpoint/`: `Mount` with a `Set`, the common endpoint set beside a service's `/api`: `/status`, `/status/health`, `/api/state` behind `Set.APIMiddleware`, and `/mcp` with the state operations.

## Index

1. `.claude/rules/packages.md`, always: package boundaries and dependencies, the dependency whitelist, fixing here and never in a consumer, and the version bump each kind of change takes.
2. `.claude/rules/deploy.md`, always: tests under `test/`, the recipes, and the release constraints.
3. `.claude/rules/envelope.md`, path scoped to `respond/`, `stream/`, `mcp/` and `test/`: what is frozen in the envelope, pagination and step stream contract.
4. Hidden docs in `.nimpress/`: `envelope.md`, the envelope, pagination and step stream contract; `packages.md`, what `stream/`, `mcp/` and `cli/` do; `standards.md` and `compliance.md`.
5. `.claude/skills/deploy`: use when the user says deploy, release or ship in this repo.
6. `.claude/skills/test`: use when testing a change; `just test` and what passes means.
