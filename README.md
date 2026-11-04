# NimBody

The shared contract of a Nimtech Go service and its cli. For servers built on Echo: request body binding and validation, query parameter validation, and the response envelope with pagination. For clis: exit codes, output, the named config with its local override and write guard, the mcp server and the step stream. Fixes land here and reach every consumer through a version bump, never through repo local copies.

## Installation

```
go get github.com/nimling/nimbody@v1.0.0
```

## Packages

### nimbody

Body binding and validation. `Bind` decodes the body into your struct while capturing which keys the caller actually sent, so partial updates only touch sent fields.

```go
type BodyEmployee struct {
    FirstName *string `json:"first_name"`
    Phone     *string `json:"phone"`
}

var req BodyEmployee
val, err := nimbody.Bind(ctx, &req)
if err != nil || val.IsEmpty() {
    return respond.Error(ctx, http.StatusBadRequest, "Malformed request")
}

fields := map[string]interface{}{}
if val.HasKey("first_name") {
    fields["first_name"] = req.FirstName
}
if denied := val.Deny("id", "sa_*"); len(denied) > 0 {
    return respond.Error(ctx, http.StatusBadRequest, "Denied fields: "+strings.Join(denied, ", "))
}
```

`HasKey` reports the key was sent, `HasValue` reports it was sent with a non nil value, `ForObject` descends into a nested object, `Deny` rejects keys by name or `prefix*` pattern, and `Body.GetUnknown` lists keys the target struct does not declare.

### nimbody/query

Query parameter validation with an explicit whitelist. Every handler that reads query parameters calls `ValidateQueryParams` before any database work.

```go
params, err := query.ValidateQueryParams(ctx, true,
    "org_id|organization_id",
    query.QueryParam{"with": {"role"}},
)
if err != nil {
    return respond.Error(ctx, http.StatusBadRequest, err.Error())
}

orgID, err := params.GetUUID("org_id")
tokens, err := params.GetWith()
```

The second argument allows the reserved pagination knobs `limit`, `page`, `order`, `column`, `cursor` alongside the whitelist. A whitelist entry is a plain name, an alias group joined with `|` that canonicalizes to the first name, or a `query.QueryParam` map declaring which `@key:value` filters a `with` token accepts. Unknown parameters, conflicting aliases, and undeclared with filters all reject with a descriptive error. Typed accessors: `Get`, `GetUUID`, `GetUUIDs`, `GetSlice`, `GetTime`, `GetWith`.

`with` tokens follow `with=contact@role:main,image`: comma separated names, each optionally carrying `@key:value` filters parsed into `WithToken{Name, Filters}`.

### nimbody/respond

The response envelope, pagination, and scope filtering. Every response body has the same shape:

```json
{
  "data": [],
  "error": { "message": "..." },
  "request_id": "uuid",
  "count": 0,
  "pagination": { "page": 1, "limit": 20, "total": 0, "prev": null, "next": null }
}
```

```go
e.Use(respond.RequestIDMiddleware())
e.JSONSerializer = &respond.FriendlyJSONSerializer{}

func GetThings(ctx echo.Context) error {
    if err := respond.InitPagination(ctx, nil, nil); err != nil {
        return respond.Error(ctx, http.StatusBadRequest, err.Error())
    }
    if err := respond.ValidateSortColumn(ctx, []string{"name"}); err != nil {
        return respond.Error(ctx, http.StatusBadRequest, err.Error())
    }

    things := make([]*db.Thing, 0)
    q := db.Instance().NewSelect().Model(&things)
    q = respond.PaginationQuery(ctx)(q)
    if err := q.Scan(ctx.Request().Context()); err != nil {
        return respond.ErrorInternal(ctx)
    }
    return respond.JSON(ctx, http.StatusOK, things, len(things))
}
```

`InitPagination` reads `limit`, `page`, `order`, `column`, `cursor` and rejects invalid input: `order` accepts only ASC and DESC in any casing, cursor pagination requires `column=id`. `PaginationQuery` applies order, limit, offset, and cursor conditions to a bun select. `JSON` attaches the pagination block with base64 cursor links when pagination was initialized. `Error`, `ErrorInternal`, and `NoContent` emit the same envelope. `FilterToScope` and `RejectOutOfScope` trim reads and reject writes against a jsonb scope key set. `Redirect`, `Passthrough`, and `Stream` cover the non envelope cases.

A request carrying `Accept: text/event-stream` turns the same handler into a step stream. `GetSteps(ctx)` opens a reporter the handler and, through `StepsFrom(ctx.Request().Context())`, the services below it report on: `Start(id, label, parentID, dependsOn...)`, `Done(id, data)`, `Fail(id, err)`. Each call is one `event: step` frame carrying the flat row `id, label, parent_id, depends_on, status, started_at, ended_at, data, error`, with `status` one of `running`, `done`, `failed`. `JSON`, `Error` and `NoContent` then end the stream with one `event: result` frame, `{ "status": <http status>, "body": <envelope> }`, and the http status is 200. Without the header the reporter records nothing and the handler answers the plain envelope, so a handler reports its steps once and every client picks the shape. The caller names itself in `User-Agent` with a product token and a comment, `sauth/1.4.2 (mcp)` for a tool run and `sauth/1.4.2` for a terminal, read with `ctx.Request().UserAgent()`.

```go
func PostThing(ctx echo.Context) error {
    steps := respond.GetSteps(ctx)
    steps.Start("validate", "Validate the body", "")
    if err := nimbody.Bind(ctx, &body); err != nil {
        steps.Fail("validate", err)
        return respond.Error(ctx, http.StatusBadRequest, err.Error())
    }
    steps.Done("validate", nil)
    steps.Start("write", "Write the thing", "", "validate")
    thing, err := service.PostThing(ctx.Request().Context(), body)
    ...
    steps.Done("write", thing.ID)
    return respond.JSON(ctx, http.StatusCreated, thing, 1)
}
```

### nimbody/stream

The client side of the step stream. `Read(res, onStep)` drains a response: a `text/event-stream` body yields each `event: step` row to `onStep` and returns the `event: result` frame as `Status` and `Body`, any other body returns as is with the http status. `Report(w)` returns the callback a cli hands to `Read`: one human line per step, or one `step <json>` line when `EnvMCP` is set, which is how a cli running under its own mcp server hands its steps up. `Line` and `Parse` are the two ends of that line. `Machine()` is true under such a run, for a cli with its own terminal output that reports steps only there.

```go
req.Header.Set("Accept", respond.MIMEEventStream)
res, err := client.Do(req)
out, err := stream.Read(res, stream.Report(os.Stderr))
```

### nimbody/mcp

The model context protocol server a cli serves on stdio and a service serves over streamable HTTP. `Server{Name, Version, Tools}.Serve(ctx, in, out)` answers `initialize`, `ping`, `tools/list`, `tools/call`, `resources/list`, `resources/read` and `resources/templates/list`. Every tool carries `_meta.ui.resourceUri` pointing at `ui://<name>/run`, the embedded run page, so a host that renders MCP Apps shows the call as a node graph of its steps in the Samna glass design and the result as a table or a key value list when it lands. The page polls the extra tool `steps`, visible to the app only, while the call runs. A call that carries `_meta.progressToken` gets one `notifications/progress` per step.

A cli registers MCP App pages of its own in `Server.Pages`, through `cli.MCPCommand(root, name, version, pages...)`, each a `func() mcp.Page` built when `<cli> mcp` starts so it can read the config the server runs under. A `Page` is `ui://<name>/<page>`: its HTML with `__NAME__` and `__VERSION__` replaced, `Frames` and `Connect` served as `_meta.ui.csp` `frameDomains` and `connectDomains`, and `Tools` served visible to the app only and run without touching the run page state. A command annotated `cli.AnnotationMCPPage: "<page>"` links its tool to that page instead of the run page. `Frame(name, title, frames, sign)` is the reusable page for a command that opens something: it reads the result JSON `{name, url, frame}`, calls the app only tool `sign` with `{frame}` when the host grants `serverTools` and frames the address it answers, or `frame` itself without `sign`, only http and https, and offers `url` in the browser through `ui/open-link`.

`Command(executable, path, description, flags)` builds the tool for one cli command: the input schema from its flags and a `Run` that execs the binary with the argv `Argv` builds, `NIMBODY_MCP=1` in its environment, stdout as the result text and every `step <json>` line on stderr as a step.

```go
server := &mcp.Server{Name: "sauth", Version: version.Get(), Tools: tools}
return server.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
```

The same server answers a remote host over streamable HTTP. A service defines each api operation once with `Define(Spec{Name, Description, Method, Path, Access, Page, Middleware}, service)`, where `service` is `func(ctx echo.Context, in In) (Out, error)`, the same function the route calls. `Route(group, operations...)` registers the echo routes, binding `In` from its `param`, `query` and `json` tags and answering the envelope; `Server{Operations, AuthorizationServer}.Mount(e, auth...)` serves the same operations as tools at `/mcp` beside `/api`, behind the auth middleware the `/api` group uses. A tool binds its typed input from the call arguments and runs the service on the request's own context, with the operation's middleware, path parameters, query and route path set as the route would see them, so claimius gates apply unchanged; a tool never calls the route. The input schema comes from `In` (the `jsonschema` tag is the description), the output schema from `Out` as `structuredContent.data`, and a read tool also takes `limit` and `cursor`. `List[T]{Rows, Total}` answers a paginated list, and a cut short tool result ends with how to narrow it. Steps a service reports through `respond.GetSteps` reach the caller as `notifications/progress` on an event stream when the call carries a progress token. `Mount` serves the OAuth protected resource metadata (RFC 9728) at `/.well-known/oauth-protected-resource` and `/.well-known/oauth-protected-resource/mcp`, naming `https://<host>/mcp` and the authorization server, and answers a call without a bearer token, or one the auth middleware refuses, with 401 and `WWW-Authenticate: Bearer resource_metadata="..."`.

```go
get := mcp.Define(mcp.Spec{Name: "article_get", Description: "Read one article", Method: http.MethodGet, Path: "/articles/:article_id", Access: mcp.AccessRead}, service.GetArticle)
mcp.Route(e.Group("/api", auth), get)
server := &mcp.Server{Name: "sauth", Version: version.Get(), Operations: []mcp.Operation{get}, AuthorizationServer: prophet}
server.Mount(e, auth)
```

### nimbody/cli

The cli layer. `Output` is the one renderer every cli shares: `Output.Flags(root)` adds `--json`, `--raw`, `--plain`, `--fields a,b.c`, `--sort field[:desc]`, `--jq <expr>`, `--template <go template|@file>`, `--limit` and `--cursor`. `Output.Print(out, errOut, body)` takes the server envelope, prints its data as a table on a terminal and json in a pipe or under mcp, applies the selection, sort, jq or template, and writes one line on stderr naming the next cursor when more rows exist. `Output.GetQuery()` is the `limit`, `cursor` and, when `ServerSort` is set, `column` and `order` to send. Template helpers are `table`, `json`, `join`, `pluck`, `timeago`, `color` (off under `NO_COLOR` and in a pipe) and `truncate`. Being persistent flags, the same options reach every generated mcp tool. `OperationCommand(operation, call)` builds the cobra command for an `mcp.Operation`, ids positional in path order and query and body fields as flags. Setting `cli.Bridge` to a function returning the service's `/mcp` url and the stored token makes `<cli> mcp` relay stdio to that endpoint through `Relay`, so a local host reaches the backend's tools with the cli's login.

### nimbody/shape

The output shaping the cli renderer and the remote mcp tools share: `Shape{Fields, Sort, JQ, Template, Colour}`, `Select` for sort and fields over decoded json, `Write` for jq (gojq) and the template.

## Consumers

`samna-bookable-server` and `nimployer` import all three packages. The envelope JSON keys and the reserved query parameter names are frozen contracts across those repos and their frontends.

## Testing

```
just test
```

Tests live under `test/` and assert the observable contracts of all three packages.

## Deploying

`just deploy` runs a patch bump through sbump. See `.claude/rules/deploy.md` and `.claude/skills/deploy/SKILL.md` for the full procedure and the minor and major variants.

## License

Apache License 2.0. See `LICENSE` and `NOTICE`.
