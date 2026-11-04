---
paths:
  - "respond/**"
  - "stream/**"
  - "mcp/**"
  - "test/**"
---

# Envelope and pagination contract

Frozen. Every consumer's frontend and OpenAPI spec parses these keys. The full contract, with defaults, cursors, links and frames: `.nimpress/envelope.md`. The bump a change takes: `packages.md` Compatibility.

1. `respond.JSON`, `respond.Error` and `respond.NoContent` emit exactly `data`, `error`, `request_id`, `count`, `pagination`.
2. An empty list is 200 with `data: []`, never 404.
3. Reserved query names: `limit`, `page`, `order`, `column`, `cursor`. Cursor mode requires `column=id`.
4. Sortable handlers call `ValidateSortColumn(ctx, allowed)` after `InitPagination`.
5. `Accept: text/event-stream` answers with http status 200 and the unchanged envelope in the final `event: result` frame.
6. A nil step reporter is a no op, so service code reports without checking.
7. The caller kind is the `User-Agent` product token and comment, `sauth/1.4.2 (mcp)` or `sauth/1.4.2`, read with `ctx.Request().UserAgent()`. No custom header; it changes no response.
