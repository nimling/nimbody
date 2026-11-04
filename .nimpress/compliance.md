---
title: Compliance checks
description: The checks the nimbody security checkpoint runs on the http contract, the step stream and the mcp server, with method, pass criterion and today's result.
visibility: hidden
tags: compliance, audit, checks, nimbody, http, envelope, mcp
lastUpdated: true
---

## Scope

These checks cover the nimbody module at commit `8c3901e`: body binding in `main.go`, `query/`, `respond/` with the envelope, pagination and the step stream, the stream client in `stream/`, the stdio mcp server in `mcp/` and the release workflow. nimbody opens no listener, so a hurl request runs against a consumer built on the commit under test, `{{HOST}}` being that service in dev user mode; a check that a consumer or the ingress owns says so. The inventory with statuses is [Standards and compliance](./standards.md), the platform map is [Compliance](/ecosystem/compliance), and the roadmap checkpoint these checks serve for every service is [Releases and compliance testing for every service](/ecosystem/roadmap/compliance-in-motion).

## HTTP contract

### C1, No body on 204

1. Standard: RFC 9110 section 15.3.5.

2. Check: `respond.NoContent` with 204 or 304 writes no envelope after the status line.

3. Method: inspect `NoContent` and send a delete that answers 204.

```
DELETE {{HOST}}/api/sync/{{SYNC_ID}}

HTTP 204
[Asserts]
bytes count == 0
```

4. Pass: `NoContent` calls `WriteHeader` only, and the response has no body and no `Content-Type`.

5. Today: `fails`, `NoContent` encodes an envelope through `write`, `respond/main.go:142`.

### C2, Accept negotiation

1. Standard: RFC 9110 section 12.5.1 and section 15.5.7.

2. Check: `Accept` is parsed as a media range list with q values, and an unsatisfiable `Accept` answers 406.

3. Method:

```
GET {{HOST}}/api/provider
Accept: application/xml

HTTP 406
```

4. Pass: 406, and `Accept: text/event-stream;q=0, application/json` answers JSON.

5. Today: `fails`, `Streaming` is a substring match, `respond/step.go:44`.

### C3, Real status in the stream result

1. Standard: RFC 9110 section 15.

2. Check: a streamed error carries the status the JSON path would send in the `result` frame.

3. Method: `just test respond` runs `TestErrorStreamsResultFrame`, `test/respond_test.go:151`.

4. Pass: the test passes and the frame holds `status` equal to the handler status.

5. Today: `passes`, `respond/step.go:126`.

### C4, Cache directives on the envelope

1. Standard: RFC 9111 section 5.2.2.

2. Check: every JSON envelope carries `Cache-Control`, `no-store` for authenticated reads.

3. Method:

```
GET {{HOST}}/api/me

HTTP 200
[Asserts]
header "Cache-Control" contains "no-store"
```

4. Pass: the header is present on every envelope.

5. Today: `fails`, `write` sets only `Content-Type`, `respond/step.go:128`.

### C5, HSTS

1. Standard: RFC 6797 section 6.1.

2. Check: every HTTPS response of a consumer carries `Strict-Transport-Security` with `max-age` of at least 31536000, set by the contract or by the ingress.

3. Method:

```
GET https://service.example.com/api/status/health

HTTP 200
[Asserts]
header "Strict-Transport-Security" matches /max-age=(3153600\d|[4-9]\d{7,}|\d{9,})/
```

4. Pass: the header is present on tst and prd.

5. Today: `fails` for the module, no security header is set, searched `Strict-Transport-Security` in `respond/`; the ingress answer is `untested`.

### C6, Problem details

1. Standard: RFC 9457 section 3.

2. Check: an error response is `application/problem+json` with `type`, `title`, `status` and `detail`.

3. Method:

```
GET {{HOST}}/api/provider?unknown=1

HTTP 400
[Asserts]
header "Content-Type" contains "application/problem+json"
jsonpath "$.status" == 400
```

4. Pass: the type and members are present.

5. Today: `fails`, the frozen envelope is `error: { "message": string }`, `respond/main.go:19`.

### C7, JSON encoding

1. Standard: RFC 8259 section 8.1.

2. Check: every envelope is UTF-8 JSON declared as `application/json; charset=UTF-8`.

3. Method: `just test respond` runs `TestJSONEnvelope`, `test/respond_test.go:23`.

4. Pass: the test passes and the header matches.

5. Today: `passes`, `respond/step.go:128`.

### C8, Request id on every response

1. Standard: ISO/IEC 27001:2022 Annex A 8.15; RFC 9562 section 5.4.

2. Check: every envelope carries a version 4 `request_id`, minted when the middleware is absent.

3. Method: `just test respond` runs `TestRequestIDMiddlewareSetsID`, `test/respond_test.go:48`, then:

```
GET {{HOST}}/api/me

HTTP 200
[Asserts]
jsonpath "$.request_id" matches /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
```

4. Pass: both hold.

5. Today: `passes`, `respond/main.go:73`, `respond/main.go:83`.

### C9, No connection specific field on HTTP/2

1. Standard: RFC 9113 section 8.2.2.

2. Check: a step stream served over HTTP/2 carries no `Connection` field.

3. Method: `hurl --http2` against the tst host:

```
GET https://service.example.com/api/provider
Accept: text/event-stream

HTTP/2 200
[Asserts]
header "Connection" not exists
```

4. Pass: no `Connection` field on the HTTP/2 response.

5. Today: `untested`, the stream sets `Connection: keep-alive`, `respond/step.go:109`.

## Pagination and links

### C10, URL safe cursor

1. Standard: RFC 4648 section 5.

2. Check: the cursor is base64url without padding.

3. Method:

```
grep -n "base64\.\(Std\|URL\|RawURL\)Encoding" respond/main.go
```

4. Pass: only `RawURLEncoding` matches.

5. Today: `fails`, `base64.StdEncoding`, `respond/main.go:192`, `respond/main.go:381`, `respond/main.go:391`.

### C11, Percent encoded link query

1. Standard: RFC 3986 section 2.1 and section 3.4.

2. Check: `pagination.next` and `pagination.prev` encode every query value and the cursor.

3. Method:

```
GET {{HOST}}/api/provider?limit=1&name=a%2Bb%26c

HTTP 200
[Asserts]
jsonpath "$.pagination.next" contains "name=a%2Bb%26c"
```

4. Pass: the link round trips the values unchanged.

5. Today: `fails`, links are concatenated from raw values, `respond/main.go:316`.

### C12, Link header

1. Standard: RFC 8288 section 3.

2. Check: a paginated list carries a `Link` header with `rel="next"` and `rel="prev"`.

3. Method:

```
GET {{HOST}}/api/provider?limit=1

HTTP 200
[Asserts]
header "Link" contains "rel=\"next\""
```

4. Pass: the header is present when `pagination.next` is.

5. Today: `fails`, links are body fields only, `respond/main.go:382`, `respond/main.go:392`.

### C13, RFC 3339 only time input

1. Standard: RFC 3339 section 5.6.

2. Check: `GetTimeParam` accepts `date-time` only and rejects zoneless, day month ambiguous and unix forms.

3. Method: `just test query` runs `TestGetTimeParamFormats`, `test/query_test.go:81`, with `01/02/2006`, `2006-01-02T15:04:05` and `1700000000` expected to fail.

4. Pass: the three inputs return an error.

5. Today: `fails`, the layout list accepts them, `query/query.go:202`, `query/query.go:235`.

## Input limits

### C14, Body size limit

1. Standard: OWASP API Security Top 10 2023 API4; OWASP ASVS 4.0.3 V13.1.3.

2. Check: `Validator` reads the body through a size limit and answers 413 above it.

3. Method:

```
grep -n "MaxBytesReader\|LimitReader" main.go
```

4. Pass: a limit wraps `ctx.Request().Body` before decoding.

5. Today: `fails`, the body is teed in full, `main.go:69`.

### C15, Page and limit bounds

1. Standard: OWASP API Security Top 10 2023 API4.

2. Check: `limit` and `page` reject zero and negatives, `limit` and the cursor `limit` have an upper bound.

3. Method:

```
GET {{HOST}}/api/provider?limit=100000000&page=-1

HTTP 400
```

4. Pass: 400 for each of `limit=0`, `limit=-1`, `page=0`, `limit` above the bound.

5. Today: `fails`, `strconv.Atoi` with no range check, `respond/main.go:167`, `respond/main.go:177`; the cursor limit is only floored, `respond/main.go:212`.

### C16, Rate limiting with 429 and Retry-After

1. Standard: RFC 6585 section 4; RFC 9110 section 10.2.3; OWASP API Security Top 10 2023 API4.

2. Check: the contract offers a 429 envelope with `Retry-After`, and a consumer answers it under burst.

3. Method:

```
grep -rn "StatusTooManyRequests\|Retry-After" --include=*.go respond main.go
```

4. Pass: a `Retry-After` writer exists and 200 rapid requests to one consumer route draw 429.

5. Today: `fails`, no match in the module.

### C17, RateLimit fields

1. Standard: IETF httpapi RateLimit header fields draft.

2. Check: a limited response carries `RateLimit` and `RateLimit-Policy`.

3. Method:

```
grep -rn "RateLimit" --include=*.go .
```

4. Pass: the fields are written next to C16.

5. Today: `fails`, no match.

### C18, Content type before decode

1. Standard: OWASP ASVS 4.0.3 V13.1.5.

2. Check: a body that is not `application/json` is rejected with 415 before decoding.

3. Method:

```
POST {{HOST}}/api/provider
Content-Type: text/plain
`{"name":"x"}`

HTTP 415
```

4. Pass: 415.

5. Today: `fails`, `Validator` decodes whatever arrives, `main.go:73`.

### C19, Idempotent retries

1. Standard: IETF httpapi Idempotency-Key header field draft.

2. Check: a retried `POST` with the same `Idempotency-Key` returns the first result.

3. Method:

```
grep -rni "idempotency" --include=*.go .
```

4. Pass: a helper reads the key and replays the stored result.

5. Today: `fails`, no match.

## Validation

### C20, Query allow list

1. Standard: OWASP ASVS 4.0.3 V5.1.3.

2. Check: an undeclared or conflicting query parameter is rejected.

3. Method: `just test query` runs `TestValidateQueryParamsRejectsUnknown`, `TestValidateQueryParamsConflictingAliases` and `TestValidateQueryParamsRejectsUndeclaredWithFilter`, `test/query_test.go:20`.

4. Pass: the tests pass.

5. Today: `passes`, `query/query.go:121`.

### C21, Sort column allow list and quoted identifiers

1. Standard: OWASP ASVS 4.0.3 V5.3.4; OWASP API Security Top 10 2023 API3.

2. Check: a sort column outside the allow list is refused, a cursor column other than `id` is refused, and every column reaches SQL through `bun.Ident`.

3. Method: `just test respond` runs `TestValidateSortColumnAllowlist`, `test/respond_test.go:78`, and:

```
grep -n "OrderExpr\|Where(" respond/main.go | grep -v "bun.Ident"
```

4. Pass: the test passes and the grep prints nothing.

5. Today: `passes`, `respond/main.go:205`, `respond/main.go:236`, `respond/main.go:278`.

### C22, Mass assignment guards

1. Standard: OWASP API Security Top 10 2023 API3.

2. Check: `Deny`, `GetUnknown`, `FilterToScope` and `RejectOutOfScope` refuse fields outside the allowed set.

3. Method: `just test` runs `TestBodyValidatorDeny`, `test/parse_test.go:83`, `TestFilterToScope` and `TestRejectOutOfScope`, `test/respond_test.go:91`.

4. Pass: the tests pass. Use in each handler is checked in the consumer's own page.

5. Today: `passes`, `main.go:33`, `respond/scope.go:19`.

### C23, Body schema validation

1. Standard: OWASP ASVS 4.0.3 V13.2.2.

2. Check: a body is validated against a declared schema before the handler reads it.

3. Method: code inspection of `main.go` and `respond/` for a schema validator.

4. Pass: one validator call precedes binding.

5. Today: `fails`, only key presence checks, `main.go:20`.

### C24, Merge patch media type

1. Standard: RFC 7396 section 4.

2. Check: `application/merge-patch+json` is recognised and null deletes the member.

3. Method:

```
grep -rn "merge-patch" --include=*.go .
```

4. Pass: the type is matched where the body is bound.

5. Today: `fails`, no match; null is told apart by hand, `main.go:25`.

## Error handling

### C25, Generic internal error

1. Standard: OWASP ASVS 4.0.3 V7.4.1.

2. Check: `ErrorInternal` returns a fixed message and the request id.

3. Method: `just test respond` runs `TestErrorEnvelope`, `test/respond_test.go:36`.

4. Pass: the message is `Internal server error`.

5. Today: `passes`, `respond/main.go:138`.

### C26, No raw library errors to the caller

1. Standard: OWASP ASVS 4.0.3 V7.4.1; ISO/IEC 27001:2022 Annex A 8.26.

2. Check: errors the module returns or streams are fixed texts, never `strconv`, `base64`, `json` or wrapped errors.

3. Method:

```
grep -n "return err$\|err.Error()\|%w" respond/main.go respond/scope.go respond/step.go
```

4. Pass: no match on a path that reaches a response.

5. Today: `fails`, `respond/main.go:172`, `respond/main.go:195`, `respond/scope.go:41`, `respond/step.go:81`.

## Step stream

### C27, Server framing

1. Standard: WHATWG HTML section 9.2.

2. Check: frames are `event` and `data` lines ended by a blank line, flushed per frame, as `text/event-stream`.

3. Method: `just test respond` runs `TestStepsStreamFramesAndResult`, `test/respond_test.go:106`.

4. Pass: the test passes.

5. Today: `passes`, `respond/step.go:104`.

### C28, Client field parsing

1. Standard: WHATWG HTML section 9.2.6.

2. Check: `stream` accepts `event:` and `data:` with or without the space after the colon and ignores comment lines.

3. Method: `just test stream` with a frame `event:step\ndata:{}\n\n` added to `TestStreamReadFrames`, `test/stream_test.go:16`.

4. Pass: the frame is read.

5. Today: `fails`, only the forms with a space match, `stream/main.go:77`.

## MCP

### C29, Protocol version negotiation

1. Standard: Model Context Protocol 2025-06-18, lifecycle, version negotiation.

2. Check: `initialize` answers a supported version, never echoes an unknown one.

3. Method:

```
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}' | ssync mcp | jq -r '.result.protocolVersion'
```

4. Pass: `2025-06-18`.

5. Today: `fails`, the client version is echoed, `mcp/main.go:290`.

### C30, JSON-RPC error codes

1. Standard: JSON-RPC 2.0 section 5.1.

2. Check: a message without `jsonrpc: "2.0"` draws `-32600`, malformed `tools/call` params and an unknown tool draw `-32602`.

3. Method:

```
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nothing"}}' '{"id":2,"method":"ping"}' | ssync mcp | jq -c '.error.code'
```

4. Pass: `-32602` then `-32600`.

5. Today: `fails`, both come back as `result` with `isError`, `mcp/main.go:330`, `mcp/main.go:340`, and `jsonrpc` is never checked, `mcp/main.go:276`.

### C31, Parse errors and notifications

1. Standard: JSON-RPC 2.0 sections 4.1 and 5.1.

2. Check: invalid JSON draws `-32700` and a notification draws no reply.

3. Method:

```
printf '%s\n' 'not json' '{"jsonrpc":"2.0","method":"notifications/initialized"}' | ssync mcp | jq -c '.error.code'
```

4. Pass: exactly one line, `-32700`.

5. Today: `passes`, `mcp/main.go:278`, `mcp/main.go:281`.

### C32, No flag smuggling through positional args

1. Standard: OWASP Top 10 2021 A03, injection, CWE 88.

2. Check: a tool call cannot pass an undeclared flag through `args`.

3. Method:

```
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"provider_get","arguments":{"args":["--endpoint=http://169.254.169.254"]}}}' | ssync mcp | jq -r '.result.isError'
```

4. Pass: `true` with a refusal, and no request leaves for the given endpoint.

5. Today: `fails`, flags are checked against the declared set, `mcp/main.go:140`, but `args` are appended unchecked, `mcp/main.go:155`.

### C33, Isolated concurrent calls

1. Standard: Model Context Protocol 2025-06-18, tools.

2. Check: two concurrent `tools/call` requests keep their own steps and result.

3. Method: code inspection of `Server.begin` and `Server.step`.

4. Pass: the run is keyed by request id.

5. Today: `fails`, one shared `run` record, `mcp/main.go:211`.

### C34, Run page message origin

1. Standard: MCP Apps extension, host messaging; OWASP ASVS 4.0.3 V14.5.

2. Check: the page posts to the host origin and drops any `message` whose `event.source` is not `window.parent`.

3. Method:

```
grep -n 'postMessage(.*"\*")\|event.source' mcp/page.html
```

4. Pass: no `"*"` target and one `event.source` check.

5. Today: `fails`, `mcp/page.html:182`, `mcp/page.html:190`. The frame page checks `event.source`, `mcp/frame.html:164`, and still posts with `"*"`, `mcp/frame.html:114`.

### C35, Run page output encoding

1. Standard: OWASP ASVS 4.0.3 V5.3.3.

2. Check: the page renders tool data through `textContent` only.

3. Method:

```
grep -n "innerHTML\|outerHTML\|insertAdjacentHTML\|document.write" mcp/page.html
```

4. Pass: no match.

5. Today: `passes`, `mcp/page.html:229`.

### C36, Typed tool input schema

1. Standard: JSON Schema 2020-12, validation vocabulary section 6.1.1.

2. Check: integer and number flags are typed as such and the schema names its dialect.

3. Method: code inspection of `Command`.

4. Pass: a `$schema` member and `integer` for int flags.

5. Today: `fails`, every other kind is `string`, `mcp/main.go:101`.

## Supply chain

### C37, Vulnerability scan and static analysis in CI

1. Standard: ISO/IEC 27001:2022 Annex A 8.28 and 8.8; CRA Annex I part I (2)(a).

2. Check: the release workflow runs `govulncheck` and `go vet` and stops on a finding.

3. Method:

```
grep -n "govulncheck\|go vet\|staticcheck" .github/workflows/release.yml
```

4. Pass: both steps run before the release step.

5. Today: `fails`, the workflow only creates the release, `.github/workflows/release.yml:16`.

### C38, Known vulnerable modules

1. Standard: CRA Annex I part I (2)(a); ISO/IEC 27001:2022 Annex A 8.8.

2. Check: no reachable known vulnerability in the module graph.

3. Method:

```
govulncheck ./...
```

4. Pass: `No vulnerabilities found`.

5. Today: `untested`; `golang.org/x/crypto v0.22.0` and `golang.org/x/net v0.24.0` are pinned, `go.mod:27`, `go.mod:28`.

### C39, Dependency allow list

1. Standard: ISO/IEC 27001:2022 Annex A 8.28; `.claude/rules/packages.md` Boundaries 4.

2. Check: the runtime packages import only stdlib, echo, google/uuid and uptrace/bun; godotenv is confined to `example/` and testify to `test/`.

3. Method:

```
grep -h '^\s*"[a-z0-9.-]*\.[a-z]*/' main.go query/*.go respond/*.go stream/*.go mcp/*.go | grep -v nimling/nimbody | sort -u
```

4. Pass: only `github.com/google/uuid`, `github.com/labstack/echo/v4` and `github.com/uptrace/bun`.

5. Today: `passes`, `go.mod:6`, `go.mod:8`, `go.mod:10`; `godotenv` and `testify` sit at `go.mod:7`, `go.mod:9` for `example/` and `test/` as `.claude/rules/packages.md` Boundaries 5 allows.

### C40, SBOM and provenance

1. Standard: CRA Annex I part II (1); ISO/IEC 27001:2022 Annex A 8.32.

2. Check: each tag publishes an SBOM and a signed build provenance.

3. Method:

```
gh release view --repo nimling/nimbody --json assets --jq '.assets[].name'
```

4. Pass: an SPDX or CycloneDX file and a provenance attestation per tag.

5. Today: `fails`, `.github/workflows/release.yml:16` publishes notes only.

## Summary

1. Checks: 40.

2. Pass: 11, C3, C7, C8, C20, C21, C22, C25, C27, C31, C35, C39.

3. Fail: 27.

4. Untested: 2, C9, C38.
