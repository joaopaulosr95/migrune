# Contributing

## Setup

Install the pinned Go, Task, staticcheck, actionlint and shellcheck versions with [mise](https://mise.jdx.dev):

```sh
task setup   # or: mise install
```

## Running the tests

Before opening a PR or pushing to `main`:

```sh
task lint test test:int:all
```

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
go test ./...
task replicaset:down   # stop it and discard its data
```

### MongoDB doesn't start on Linux 6.19 or later

On some hosts running Linux 6.19 or later, some MongoDB builds exit at
startup with "Linux kernel versions 6.19 and newer has a known
incompatibility with this version of MongoDB"
([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)). On GitHub's
Ubuntu runners the 8.0 and 8.3 images do, while 7.0, 8.2 and 9.0 start. It
depends on the host, not only on the kernel version: the same 8.0 and 8.3
images start on other Linux 6.19+ hosts. It can affect the test replica set
in `test/replicaset/`, so `task test:int:all`.

Use an image that starts on that host, for example
`MONGO_IMAGE=mongo:8.2 task test:int` for the test replica set, or another
host.

If you change a command or flag, run `task docs` and commit the regenerated
`docs/reference/cli/`.

## Commits and pull requests

`main` has a linear history of signed commits: no merge commits. Work lands
on it in one of two ways:

- **A pull request**, squash-merged: the PR title becomes the commit message
  on `main`.
- **A commit pushed straight to `main`**, by a maintainer: rebase it on the
  latest `main` first, so it fast-forwards, and run
  `task lint test test:int:all` before pushing, since nothing reviews it.

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
for a major release, edit them once the Release workflow has finished
(`gh release edit vX.Y.Z --notes-file notes.md`).
Re-running the workflow doesn't overwrite them.

Before pushing a release:

- `task lint test test:int:all` passed on the commit being released, on all
  four images (CI runs only three).
- For a release with breaking changes or a MongoDB major dropped, the notes
  say so.
