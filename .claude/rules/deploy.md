# Testing and releasing

This repo publishes one artefact: the Go module consumed via `go get github.com/nimling/nimbody`. The release procedure is the `deploy` skill and the test run the `test` skill, under `.claude/skills/`. The constraints below apply outside of any single deploy run.

## Every change

1. Update or add Go tests under `test/` for any change that has an observable contract.
2. Use the existing `justfile` recipes: `just test`, `just build`, `just deploy`. Direct `go test`, `go build`, or `sbump` invocation drifts from the contract, except the minor or major bump below.

## Releasing

1. Never edit `APP_VERSION` in `.env` by hand. `sbump` owns that field. Hand edits drift from the tag and corrupt the bump sequence.
2. Never tag manually with `git tag` or `git push --tags`. `just deploy` writes the tag and pushes it as one operation via `sbump`.
3. Never use `--no-verify` or `--no-gpg-sign` on commits going out as part of a release. Hooks and signing are part of the contract.
4. Never skip `just test` or `just build` before `just deploy`. A red build or a failing test must block the bump.
5. Never commit generated or unrelated files together with a release. Stage only the source changes and the test changes that describe the release.
6. The commit message is one line that describes the user visible change. The release notes derive from `git log` after the tag.
7. `just deploy` does a patch bump. For minor or major, run `sbump minor|major --env APP_VERSION --push-version --auto --workflow` instead. The repo does not gate this; the human running the deploy picks the right bump by `.claude/rules/packages.md` Compatibility.
8. Consumers pick it up with `go get github.com/nimling/nimbody@vX.Y.Z`.
