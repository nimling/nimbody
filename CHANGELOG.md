# Changelog

## v1.0.0

The first public release.

1. `nimbody`: request body binding that records which keys were sent, for partial updates, with `HasKey`, `HasValue`, `ForObject`, `Deny`, `IsEmpty` and `GetUnknown`.
2. `query`: validation of query parameters, pagination and cursors.
3. `respond`: one response envelope with data, error, pagination and request id, and a step stream over server sent events that ends in a result frame.
4. `cli`: the shared command line layer: exit codes, output as table, plain text or json (json when piped), output templates, named configs with a local override, env files and a write guard, and install commands for a Claude Code plugin and skill.
5. `mcp`: every command served as an mcp tool over stdio and streamable http, with tool annotations, inline images and an MCP App page.
6. `stream`: a loopback login page for clis and the client for the step stream.
