---
name: test
description: Runs the nimbody Go tests under test/, which cover body binding, query parameters, the response envelope and pagination, the step stream and the mcp server. Use when asked to test, verify or prove a change in this repo. Not for releasing, which is the deploy skill.
allowed-tools: Bash(just *)
---

The tests of this library. Releasing is the `deploy` skill; where tests go is `.claude/rules/deploy.md` Every change 1.

## Run

1. `just test` runs `go test ./test/...`. No database, no network, no env keys.
2. The files are `test/parse_test.go` (root body binding), `test/query_test.go` (`query/`), `test/respond_test.go` (`respond/`, the envelope, pagination and cursors), `test/stream_test.go` (`stream/` and the step stream), `test/mcp_test.go` (`mcp/` on stdio), `test/remote_test.go` (`mcp/` over http, `Define`, the bridge and `OperationCommand`), `test/output_test.go` (`cli.Output` and `shape/`), `test/cli_test.go` and `test/config_test.go` (`cli/`).
3. One test: `just test <TestName>` runs the same suite with `-run <TestName>`.
4. Passes means `ok` for `./test` and exit 0. A failure prints the test name and the file line; report them and stop.

## What a change needs

1. A change with an observable contract adds or extends a test under `test/` that hits the public function and asserts the returned value or the error sentinel.
2. A change to the envelope, pagination or step stream is checked against `.claude/rules/envelope.md` as well as the tests.
3. Before a release, the consumers' call sites of every touched symbol: `.claude/rules/packages.md` Compatibility 4.
