---
name: deploy
description: Releases nimbody. Tests, builds, commits, pushes and tags through sbump so consumers pick the new version up with go get. Use when the user says deploy, release or ship in this repo, or asks for a patch, minor or major release of nimbody. Not for running the tests alone, which is the test skill, and not for the sbump flags themselves, which is the sbump skill.
argument-hint: [patch|minor|major]
allowed-tools: Bash(just *), Bash(git *), Bash(sbump *)
---

The release of `github.com/nimling/nimbody`, in order, stopping on the first failure with the error verbatim. Tests are the `test` skill; the standing rules are `.claude/rules/deploy.md`; the bump each change takes is `.claude/rules/packages.md` Compatibility.

## 1. Tests for the change

1. Read the change with `git diff` and `git status` and name its observable surface.
2. Extend the matching file under `test/` (`parse_test.go`, `query_test.go`, `respond_test.go`, `stream_test.go`, `mcp_test.go`, `remote_test.go`, `output_test.go`) or add one. Trivial moves and rename only diffs need none.
3. Anything that changes a return value, a sentinel error, a method signature, an enum or a struct field is covered, by hitting the public function and asserting the value or the sentinel.

## 2. Test and build

1. `just test`. Failure: report the failing test names and stop. No commit, no bump.
2. `just build` runs `go build ./...`. Failure: report the build output and stop.
3. Check the call sites of every touched symbol in the consumers `CLAUDE.md` names.

## 3. Commit and push

1. Stage the source and test files of the release by name with `git add <files>`, never another session's work or generated files. When the tree holds changes that are not the release, release from a clean clone, because `sbump` refuses a dirty tree.
2. `git commit -m "<one line>"` describing the user visible change, `#<number>: <description>` when an issue covers it; the release notes come from `git log` after the tag.
3. `git push` on the current branch. Nothing to commit: skip the commit and still push.

## 4. Bump and tag

1. Patch: `just deploy`, which runs `sbump patch --env APP_VERSION --push-version --auto --workflow`: version write, tag, push, workflow track in one run.
2. Minor or major: `sbump minor --env APP_VERSION --push-version --auto --workflow`, or `major`, picked by `.claude/rules/packages.md` Compatibility; `just deploy` always bumps a patch.
3. Success is the tag on `origin`. Consumers pick it up with `go get github.com/nimling/nimbody@vX.Y.Z`; an envelope, pagination, cursor or reserved query change is a major upgraded in every consumer in the same change set.

## Never

1. Skip step 2. A missing test, a failing test or a failing build blocks the release.
2. Hand edit `APP_VERSION` in `.env`; `sbump` owns it.
3. Use `--no-verify` or `--no-gpg-sign`.
4. Tag with `git tag` or `git push --tags`; `sbump` writes and pushes the tag.
