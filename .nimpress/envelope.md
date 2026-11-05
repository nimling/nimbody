---
title: Envelope, pagination and step stream
description: The response envelope, the pagination block and cursors, and the step stream every nimbody consumer's frontend and OpenAPI spec parses.
visibility: hidden
tags: nimbody, envelope, pagination, cursor, step stream, server sent events, request id
lastUpdated: true
---

## Envelope

1. `respond.JSON`, `respond.Error` and `respond.NoContent` emit exactly `data`, `error` (omitted when nil, else `{ "message": string }`), `request_id`, `count`, and `pagination` (omitted unless initialized).

2. Errors carry `data: []` and `count: 0`, unless `Error` is passed data: then `data` is that value and `count` its length, 1 for a single object. An empty list is 200 with `data: []`, never 404.

3. `request_id` comes from the context key set by `RequestIDMiddleware`; the responders mint a fresh uuid when the middleware is absent.

## Pagination

1. Reserved query names: `limit`, `page`, `order`, `column`, `cursor`. `ValidateQueryParams` skips them when paginated is true; `WithPagination` drops them from rebuilt links.

2. `InitPagination` defaults to page 1, limit 20, order DESC, column id. `order` is ASC or DESC in any casing; `limit` and `page` must be integers.

3. Cursor mode requires `column=id`. The cursor is base64 JSON `PageCursor{column, order, id, limit, page}`; a limit of 0 or below falls back to 20.

4. Sortable handlers call `ValidateSortColumn(ctx, allowed)` after `InitPagination`; `id` always passes.

5. `PaginationQuery` filters on id relative to the cursor in cursor mode, and applies order, limit and offset in page mode.

6. `pagination.next` is set when `page * limit < total`, `pagination.prev` when the request carried a cursor. Both are the path plus a base64 cursor, keeping the non reserved query params.

## Step stream

1. `Accept: text/event-stream` on any handler that answers through `JSON`, `Error` or `NoContent` gives `text/event-stream` with http status 200. The envelope is unchanged inside the final frame.

2. `event: step` frames carry a `Step`: `id`, `label`, `parent_id`, `depends_on`, `status` (`running`, `done`, `failed`), `started_at`, `ended_at`, `data`, `error`. One frame is sent per `Start`, `Done` or `Fail`.

3. `event: result` is the last frame: `{ "status": <the http status the JSON path would have sent>, "body": <envelope> }`.

4. `GetSteps(ctx)` opens the reporter once per request and puts it on the request context for `StepsFrom`. A nil reporter is a no op, so service code reports without checking.

4.1. Every stream answers the `X-Frame-Limit` response header, the longest line in bytes a reader must accept, sent once before the first frame because a frame is one `data:` line and the result frame carries the whole envelope. A route declares its own with `FrameLimitMiddleware(bytes)`, `0` for unbounded; a route that declares nothing announces `FrameLimit`, 16 MiB. `stream.Read` sizes its scanner from the header and falls back to `FrameLimit` when the header is missing. `POST /state/export` declares `0`, as its result frame holds every state line with its blobs.

5. The caller kind is the `User-Agent` product token and comment, `sauth/1.4.2 (mcp)` or `sauth/1.4.2`, read with `ctx.Request().UserAgent()`. There is no custom header, and the caller kind changes no response.
