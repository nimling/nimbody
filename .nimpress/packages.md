---
title: Packages
description: What the stream, mcp and cli packages of nimbody do for a service and for a company cli.
visibility: hidden
tags: nimbody, packages, stream, mcp, cli, config, output
lastUpdated: true
---

## stream

The client side of the step stream. `EnvMCP` is the variable an mcp tool run sets on the cli it runs. `GetLoginPage` renders the page a cli's login loopback answers with, from an embedded page on the Mar tokens, with the product's name and mark from `LoginPage.Product`.

## mcp

The stdio model context protocol server every cli serves through, with an embedded run page and an embedded frame page.

1. A `Page` is an MCP App resource `ui://<cli>/<name>` with its CSP domains and its app only tools. A `Tool` whose `Page` names it links its result there instead of the run page.

2. Over streamable HTTP, `Define` is the one operation definition of a service, `Route` its echo routes, and `Server.Mount` mounts its tools at `/mcp` beside `/api`, behind the same auth middleware, with the RFC 9728 protected resource metadata and the 401 `WWW-Authenticate` challenge. A tool runs the service function the route runs, on the request's own context.

## cli

The shared cli layer the `cli` skill describes.

1. `Execute` runs with the family exit codes and `Error`. `Print` and `FormatFor` print a table on a terminal, json in a pipe and under mcp, and `FormatPlain` tab separated lines on request.

2. `Tools`, `Access` and `MCPCommand` generate annotated mcp tools from the cobra tree. A command annotated `AnnotationMCPPage` links its tool to a page the cli passes to `MCPCommand`.

3. `SkillCommand` serves the skill, and `SkillTreeCommand` a cli with references or several skills. `PluginCommand` has `install` for Claude Code, `desktop` for Claude Desktop and `status` for drift.

4. `Settings` is the config. `Flags` adds `--config`, `--env`, `--global`, `--yes` and `--plan`. `Load` layers the flag, the environment, `--env` files, the local `.<cli>/config.yaml` found walking up and the named config in `~/.<cli>/config.yaml` into the environment; the local config takes over from the named one. `Guard` asks before a write on a `protected` config. `ConfigCommand` is `config get`, `list`, `set`, `use`, `init`, `local` and `path`.

5. `Output` is the shared renderer with `--json`, `--raw`, `--plain`, `--fields`, `--sort`, `--jq`, `--template`, `--limit` and `--cursor`. `OperationCommand` builds a command from an `mcp.Operation`. `Bridge` and `Relay` make `<cli> mcp` relay stdio to the backend `/mcp` with the stored login.
