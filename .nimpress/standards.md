---
title: Standards and compliance
description: Which RFCs, specifications and compliance duties the nimbody http contract and its mcp server implement, read from the code.
visibility: hidden
tags: compliance, standards, rfc, http, api, nimbody, mcp
lastUpdated: true
---

## Scope

This page covers the nimbody module only: body binding in `main.go`, `query/`, `respond/` with the envelope, pagination and the step stream, the stream client in `stream/`, and the stdio mcp server in `mcp/`. nimbody opens no listener and terminates no TLS, so transport standards are owned by the consumers and the platform ingress. Statuses are read from the code at commit `8c3901e`. The platform wide pages are [Compliance](/ecosystem/compliance) and [Standards](/ecosystem/standards).

## RFCs

### RFC 9110, HTTP semantics

1. Status: `partial`

2. Evidence: the handler status code is passed through to the response, `respond/step.go:129`; `Content-Type` is set on every envelope, `respond/step.go:128`; content negotiation on `Accept`, `respond/step.go:44`; the caller kind is the `User-Agent` product token and comment, `.claude/rules/envelope.md:19`.

3. Gap: `Accept` is matched by substring with no q values and no 406, a streamed response always answers 200 with the real status only inside the `result` frame, and `NoContent` writes an envelope body after the status line, which net/http refuses for 204 and 304, `respond/main.go:142`.

### RFC 9111, HTTP caching

1. Status: `partial`

2. Evidence: `Cache-Control: no-cache` on the step stream only, `respond/step.go:108`.

3. Gap: the JSON envelope sets no `Cache-Control`, `ETag` or `Last-Modified` and no conditional request is handled; searched `ETag`, `If-None-Match`, `Cache-Control`.

### RFC 9112, HTTP/1.1

1. Status: `not applicable`

2. Evidence: message framing is net/http under Echo; the module only sets `Connection: keep-alive` on the stream, `respond/step.go:109`.

### RFC 9113, HTTP/2

1. Status: `not applicable`

2. Evidence: no listener in the module; the `Connection` header set at `respond/step.go:109` is a connection specific field RFC 9113 section 8.2.2 forbids, and only HTTP/1.1 uses it.

### RFC 9114, HTTP/3

1. Status: `not applicable`

2. Evidence: no listener in the module.

### RFC 8446, TLS 1.3

1. Status: `not applicable`

2. Evidence: no `crypto/tls` import and no listener; searched `tls`.

### RFC 9325, TLS recommendations (BCP 195)

1. Status: `not applicable`

2. Evidence: no TLS configuration in the module; searched `tls`.

### RFC 8996, Deprecating TLS 1.0 and 1.1

1. Status: `not applicable`

2. Evidence: no TLS configuration in the module; searched `tls`, `MinVersion`.

### RFC 6797, HTTP Strict Transport Security

1. Status: `missing`

2. Evidence: searched `Strict-Transport-Security`, `hsts`; no security header is set by the contract.

3. Gap: the shared response contract sets no HSTS header, so it depends on each consumer or the ingress.

### RFC 9457, Problem details for HTTP APIs

1. Status: `missing`

2. Evidence: errors use the envelope `error: { "message": string }` with `application/json`, `respond/main.go:19`, `respond/main.go:127`; searched `application/problem`, `problem`.

3. Gap: no `type`, `title`, `status`, `detail` or `instance` members and no `application/problem+json`; the envelope is frozen by `.claude/rules/envelope.md`.

### RFC 6585, Additional HTTP status codes (429) and Retry-After

1. Status: `missing`

2. Evidence: searched `429`, `TooManyRequests`, `Retry-After`; no rate limiter and no `Retry-After` writer.

3. Gap: no 429 path and no `Retry-After` helper in the contract.

### RateLimit header fields (IETF httpapi draft)

1. Status: `missing`

2. Evidence: searched `RateLimit`, `ratelimit`.

3. Gap: no `RateLimit` or `RateLimit-Policy` fields.

### RFC 8259, JSON

1. Status: `built`

2. Evidence: `encoding/json` decode of the body, `main.go:73`; envelope encode as `application/json; charset=UTF-8`, `respond/step.go:128`; `FriendlyJSONSerializer`, `respond/json.go:10`.

### RFC 6901, JSON Pointer

1. Status: `missing`

2. Evidence: searched `pointer`, `json-pointer`; nested access is `ForObject`, `main.go:54`.

3. Gap: no pointer syntax for addressing fields or reporting validation errors.

### RFC 6902, JSON Patch

1. Status: `missing`

2. Evidence: searched `json-patch`, `application/json-patch+json`.

3. Gap: no patch document type or operations.

### RFC 7396, JSON Merge Patch

1. Status: `partial`

2. Evidence: key presence and explicit null are told apart by `HasKey` and `HasValue`, `main.go:20`, `main.go:25`, with nested objects through `ForObject`, `main.go:54`.

3. Gap: the merge semantics are applied by each handler by hand and `application/merge-patch+json` is not recognised.

### RFC 9535, JSONPath

1. Status: `missing`

2. Evidence: searched `jsonpath`, `$.`.

3. Gap: no JSONPath query support.

### RFC 8288, Web linking

1. Status: `missing`

2. Evidence: pagination links are body fields `pagination.prev` and `pagination.next`, `respond/main.go:382`, `respond/main.go:392`; searched `Link`.

3. Gap: no `Link` header with `rel="next"` or `rel="prev"`.

### RFC 3986, URI generic syntax

1. Status: `partial`

2. Evidence: pagination links are built by string concatenation of path, cursor and raw query values, `respond/main.go:316`, `respond/main.go:382`, `respond/main.go:392`.

3. Gap: query values and the base64 cursor are not percent encoded, so a `+`, `/`, `=`, `&` or space in a value breaks the link.

### RFC 4648, Base16, Base32 and Base64 encodings

1. Status: `partial`

2. Evidence: the cursor is `base64.StdEncoding`, `respond/main.go:192`, `respond/main.go:381`, `respond/main.go:391`.

3. Gap: a cursor carried in a URL should use base64url (section 5); the standard alphabet emits `+` and `/`, and `+` decodes to a space in a query string.

### RFC 9562, UUIDs

1. Status: `built`

2. Evidence: request ids are `uuid.New()` (version 4) from google/uuid v1.6.0, `respond/main.go:73`, `respond/main.go:83`; ids are parsed with `uuid.Parse`, `query/query.go:45`, `query/query.go:72`.

### RFC 3339, Date and time on the internet

1. Status: `partial`

2. Evidence: output times are `time.Time` marshalled as RFC 3339, `respond/step.go:23`; input accepts `time.RFC3339Nano` and `time.RFC3339` first, `query/query.go:202`.

3. Gap: input also accepts RFC 1123, RFC 822, zoneless and ambiguous `01/02/2006` and `02/01/2006` layouts plus bare unix seconds or milliseconds, `query/query.go:202`, `query/query.go:235`, so the accepted form is not RFC 3339 only.

### RFC 9421, HTTP message signatures

1. Status: `missing`

2. Evidence: searched `Signature`, `Signature-Input`.

3. Gap: no request or response signing.

### RFC 9530, Digest fields

1. Status: `missing`

2. Evidence: searched `Content-Digest`, `Repr-Digest`, `Digest`.

3. Gap: no body integrity fields.

### Idempotency-Key header field (IETF httpapi draft)

1. Status: `missing`

2. Evidence: searched `Idempotency-Key`, `idempotency`.

3. Gap: no idempotency key handling for retried writes.

### RFC 6455 and RFC 8441, WebSocket

1. Status: `not applicable`

2. Evidence: no websocket code; progress uses server sent events, `respond/step.go:104`; searched `websocket`, `Upgrade`.

### RFC 6265, Cookies

1. Status: `not applicable`

2. Evidence: the module reads and sets no cookies; searched `cookie`, `SetCookie`.

### RFC 7239, Forwarded

1. Status: `not applicable`

2. Evidence: the module reads no client address; searched `Forwarded`, `X-Forwarded`, `RealIP`.

### RFC 6838, Media type registration

1. Status: `built`

2. Evidence: registered types `application/json`, `respond/step.go:128`, and `text/event-stream`, `respond/step.go:15`; `text/html;profile=mcp-app` for the run page and every page a cli registers, as the MCP Apps extension names it, `mcp/main.go:30`.

### RFC 9116, security.txt

1. Status: `not applicable`

2. Evidence: the module serves no routes of its own; searched `security.txt`, `.well-known`.

## Specifications

### WHATWG HTML, Server Sent Events

1. Status: `partial`

2. Evidence: server frames `event: <name>\ndata: <json>\n\n` flushed per frame with `text/event-stream`, `respond/step.go:104`; client parser for `event` and `data` lines with multi line data joined by newline, `stream/main.go:31`.

3. Gap: no `id`, `retry` or heartbeat comments and no `Last-Event-ID` resume, and the client only recognises `event: ` and `data: ` with a space after the colon, `stream/main.go:77`.

### OpenAPI 3.1

1. Status: `missing`

2. Evidence: searched `openapi`, `swagger`; no spec generation or envelope schema in the module.

3. Gap: each consumer describes the frozen envelope in its own spec by hand.

### JSON Schema 2020-12

1. Status: `partial`

2. Evidence: tool `inputSchema` with `type`, `properties`, `items` and `additionalProperties: false`, `mcp/main.go:107`.

3. Gap: no `$schema` is declared and every non bool, non slice flag is typed `string`, so integer flags lose their type, `mcp/main.go:101`.

### JSON-RPC 2.0

1. Status: `partial`

2. Evidence: `jsonrpc: "2.0"` on every message, `mcp/main.go:256`; parse error `-32700`, `mcp/main.go:278`; notifications without `id` get no reply, `mcp/main.go:281`; method not found `-32601`, `mcp/main.go:365`.

3. Gap: no `-32600` check on the `jsonrpc` member and no `-32602`; malformed `tools/call` params and an unknown tool come back as a result with `isError` instead of an error, `mcp/main.go:330`, `mcp/main.go:340`.

### Model Context Protocol

1. Status: `partial`

2. Evidence: protocol version `2025-06-18`, `mcp/main.go:26`; stdio transport as newline delimited JSON on `Serve(ctx, in, out)`, `mcp/main.go:246`; `initialize`, `ping`, `tools/list`, `tools/call`, `resources/list`, `resources/read`, `resources/templates/list`, `mcp/main.go:285` to `mcp/main.go:327`; `notifications/progress` per step when `_meta.progressToken` is sent, `mcp/main.go:354`; resource not found `-32002`, `mcp/main.go:318`.

3. Gap: `initialize` echoes whatever version the client sends without checking it is supported, `mcp/main.go:290`; streamable HTTP transport, `notifications/cancelled`, `structuredContent`, `outputSchema`, tool annotations, prompts and logging are absent, and concurrent calls share one `run` record, `mcp/main.go:211`.

### MCP Apps (the `ui` extension)

1. Status: `partial`

2. Evidence: every tool carries `_meta.ui.resourceUri` to `ui://<name>/run`, or to `ui://<name>/<page>` when its command names a page through `cli.AnnotationMCPPage`, `mcp/main.go:252`; the app only `steps` tool and the app only tools of a page with `_meta.ui.visibility: ["app"]`, `mcp/main.go:257`, `mcp/main.go:265`; one `ui://` resource per page with `text/html;profile=mcp-app`, `prefersBorder` and `_meta.ui.csp` `frameDomains` and `connectDomains` from `Page.Frames` and `Page.Connect`, `mcp/main.go:225`, `mcp/main.go:369`; `mcp.Frame` builds the reusable frame page, `mcp/frame.html`, which frames the `frame` address of a result, after an optional app only tool signs it, and opens its `url` with `ui/open-link`, `mcp/main.go:214`; the page speaks `ui/initialize`, `ui/notifications/initialized`, `tool-input`, `tool-result`, `tool-cancelled`, `host-context-changed`, `size-changed` and `ui/resource-teardown`, `mcp/page.html:201` to `mcp/page.html:430`; all rendering through `textContent`, `mcp/page.html:229`.

3. Gap: the run page posts with target origin `*` and accepts any `message` event without checking `event.source` is the host, `mcp/page.html:182`, `mcp/page.html:190`; the frame page checks `event.source`, `mcp/frame.html:164`, and posts with `*`, `mcp/frame.html:114`.

### OWASP API Security Top 10 2023

1. Status: `partial`

2. Evidence: API3 property level authorization and mass assignment tools `Deny`, `GetUnknown`, `FilterToScope`, `RejectOutOfScope`, `main.go:33`, `main.go:121`, `respond/scope.go:8`, `respond/scope.go:19`; query whitelist rejecting unknown and conflicting parameters, `query/query.go:121`; sort column allowlist, `respond/main.go:236`; identifiers quoted through `bun.Ident`, `respond/main.go:278`.

3. Gap: API4 is open: the body is read in full with no size limit, `main.go:69`, `limit` has no upper bound and `page` and `limit` accept zero and negatives, `respond/main.go:167`, `respond/main.go:177`, and there is no rate limiting; mass assignment checks are opt in per handler.

### OWASP ASVS 4.0.3, V5 validation and V13 API

1. Status: `partial`

2. Evidence: V5.1 allow list input validation on query names and `with` filters, `query/query.go:121`; typed parsing of UUIDs and times, `query/query.go:40`, `query/query.go:225`; V5.3 output encoding with `SetEscapeHTML(false)` for JSON, `respond/step.go:131`; V13.1 one envelope and `request_id` on every response, `respond/main.go:103`.

3. Gap: no schema validation of bodies (V13.2), no content type check on input before decoding, and no size limit on bodies (V13.1).

## Compliance duties

1. ISO/IEC 27001:2022 Annex A 8.26, application security requirements: `partial`. The contract is written down and frozen in `.claude/rules/envelope.md` and `.claude/rules/packages.md`, with validation primitives in `main.go` and `query/query.go`; no requirement sets body size, rate or pagination limits.

2. ISO/IEC 27001:2022 Annex A 8.28, secure coding: `partial`. Dependency allow list of stdlib, echo, google/uuid and bun, `.claude/rules/packages.md` Boundaries 4; contract tests under `test/`; parameterised SQL identifiers, `respond/main.go:278`. No static analysis or `govulncheck` step in `.github/workflows/release.yml`.

3. ISO/IEC 27001:2022 Annex A 8.15, logging: `partial`. A per request `request_id` is minted and returned in every envelope, `respond/main.go:70`, `respond/main.go:108`; the module itself logs only pagination warnings, `respond/main.go:194`, `respond/main.go:359`. Access logs and correlation of `request_id` with telemetry are left to the consumers.

4. Error leakage: `partial`. `ErrorInternal` returns the fixed text `Internal server error`, `respond/main.go:138`. `Error` sends any message verbatim, `respond/main.go:131`, and the module's own errors reach clients raw: `strconv.Atoi` and base64 or JSON decode errors from `InitPagination`, `respond/main.go:172`, `respond/main.go:195`, `decode scope: ...`, `respond/scope.go:41`, and `Steps.Fail` streams `err.Error()` to the caller, `respond/step.go:81`.
