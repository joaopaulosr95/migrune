# Contributing

## Setup

Install the pinned Go, Task, staticcheck, actionlint, shellcheck and
govulncheck versions with [mise](https://mise.jdx.dev):

```sh
task setup   # or: mise install
```

## Running the tests

Before opening a PR or pushing to `main`:

```sh
task lint test vulncheck test:int:all
```

`task vulncheck` runs `govulncheck ./...`. It's a task of its own, not part
of `task lint`, because it downloads the Go vulnerability database. CI runs
it too. A new advisory can make it fail with no change to the code; the fix
is then usually a bump of Go or of a module.

`task test:int` needs Docker. It starts the 3-node replica set in
`test/replicaset/`, waits for a primary and runs every test with
`MIGRUNE_TEST_URI` pointing at it. It uses `mongo:9` by default, and
`MONGO_IMAGE=mongo:7.0 task test:int` runs it on another image.
`task test:int:all` runs it on every tested image in turn (see
[Requirements](README.md#requirements) for the support policy and the
images), stopping at the first failure, and leaves `mongo:9` running. Run it
before every change that touches server-facing behaviour: CI runs only the
images that start on its runners (see
[MongoDB doesn't start on Linux 6.19 or later](#mongodb-doesnt-start-on-linux-619-or-later)). To manage the replica set yourself:

```sh
task replicaset:up     # start it and wait for a primary
export MIGRUNE_TEST_URI='mongodb://127.0.0.1:27017,127.0.0.1:27018,127.0.0.1:27019/?replicaSet=rs0'
go test -p 1 ./...     # one package at a time: tests share server-wide fail points
task replicaset:down   # stop it and discard its data
```

Integration test packages must run one at a time (`-p 1`): some tests change
server-wide state (fail points, a secondary's replication, the replica set
config, a step-down) that would break another package's tests. Each test
process registers in the `migrune_inttest` database; one that finds another
process registered fails its integration tests with a message saying so.
A new package with integration tests calls `inttest.Exclusive` before it
touches the server, and `inttest.Release` from its `TestMain`.

### MongoDB doesn't start on Linux 6.19 or later

On some hosts running Linux 6.19 or later, some MongoDB builds exit at
startup with "Linux kernel versions 6.19 and newer has a known
incompatibility with this version of MongoDB"
([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)). On GitHub's
Ubuntu runners the 8.0 and 8.3 images do, while 7.0, 8.2 and 9.0 start. It
depends on the host, not only on the kernel version: the same 8.0 and 8.3
images start on other Linux 6.19+ hosts. It can affect the test replica set
in `test/replicaset/`, so `task test:int:all`, and
`examples/docker-compose.yaml`.

Use an image that starts on that host, for example
`MONGO_IMAGE=mongo:8.2 task test:int` for the test replica set, or another
host.

If you change a command or flag, run `task docs` and commit the regenerated
`docs/reference/cli/`.

## Changelog and lock formats

Changelog entries and lock documents carry a format version, `v`
(`runner.EntryVersion`, `runner.LockVersion`), and every run refuses a
database holding a newer one (see
[format versions](docs/reference/changelog-and-lock.md#format-versions)).
`TestFormatGolden` in `internal/runner` pins their shape in
`internal/runner/testdata/format/`. When it fails, decide whether `v` must go
up, then rerun it with `go test ./internal/runner -run TestFormatGolden -update`:

- adding an optional field that older runners can safely ignore keeps `v`;
- changing a field's meaning, making a field required, or changing how reads
  filter entries or lock documents raises `v`, which is a major release.

`v` doesn't cover the migration file schema. Its guard is that the fixtures in
`internal/migration/testdata/valid/` are never edited, only added to, so a
file that loaded before keeps loading.

## Commits and pull requests

`main` has a linear history of signed commits: no merge commits. Work lands
on it in one of two ways:

- **A pull request**, squash-merged: the PR title becomes the commit message
  on `main`.
- **A commit pushed straight to `main`**, by a maintainer: rebase it on the
  latest `main` first, so it fast-forwards, and run
  `task lint test vulncheck test:int:all` before pushing, since nothing reviews it.

Either way, the message on `main` must be a
[Conventional Commit](https://www.conventionalcommits.org), such as
`feat(runner): add lock and changelog` or `fix: redact proxy passwords`.
Releases and their version numbers are derived from these messages.

## Releases

Every push to `main` runs semantic-release, which derives the next version
from the commits since the last `v*` tag; the first release, with no tag
before it, was 1.0.0. Don't hand-tag a release, or semantic-release sees it
as already released and publishes no images for it.

A commit marked breaking (`feat!:`, or a `BREAKING CHANGE:` footer) cuts a
major release. semantic-release writes the
release notes from the commits. When users need more, such as upgrade notes
for a major release, edit them once the Release workflow has finished, the
image job included (`gh release edit vX.Y.Z --notes-file notes.md`).
Re-running the workflow doesn't overwrite them.

Before pushing a release:

- `task lint test vulncheck test:int:all` passed on the commit being released, on all
  four images (CI runs only three).
- The examples pin the latest release (Renovate opens the pull request).
- For a release with breaking changes or a MongoDB major dropped, the notes
  say so.

semantic-release and its plugins are installed with `npm ci` from
`.github/release/package-lock.json`, which Renovate keeps up to date.

The Release workflow's `image` job builds and pushes the version's images and
their [attestations](docs/running.md#get-the-binary). If it fails after the
release was cut, publish them again: run the Release workflow by hand
(Actions, Release, "Run workflow", or
`gh workflow run release.yml -f tag=v1.2.3`) with the release's tag. It
runs only the `image` job, on that tag's checkout, and pushes the same tags
as a release does, `latest` included, so it refuses any tag but the latest
release.
