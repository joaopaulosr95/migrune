# migrune

migrune runs MongoDB migrations. Migrations are Extended JSON files kept next
to your code.

- **Data migrations**: `updateOne`, `updateMany` and `deleteMany` steps in
  one transaction.

See [design](docs/design.md) for why migrune works the way it does.

## Requirements

migrune supports MongoDB 7.0 or later, running as a replica set (even of
one member), since it needs transactions. Sharded clusters aren't supported.

Every MongoDB major from 7.0 is supported until it reaches end of life on
[MongoDB's lifecycle schedule](https://www.mongodb.com/legal/support-policy/lifecycles);
then a release drops it. The integration tests run on these images:

| Image | Why | CI, on every change and before each release | Locally, before each release |
| --- | --- | --- | --- |
| `mongo:7.0` | 7.0, the long-term-support line (also the newest 7.x) | yes | yes |
| `mongo:8.0` | 8.0, the long-term-support line | no: `mongo:8.2` runs instead | yes |
| `mongo:8` | the newest 8.x | no | yes |
| `mongo:9` | the newest 9.x | yes | yes |

CI gates every release on `mongo:7.0`, `mongo:8.2` and `mongo:9`: the 8.0
and newest 8.x images don't start on its runners
([why](CONTRIBUTING.md#mongodb-doesnt-start-on-linux-619-or-later)). The
maintainer runs the whole suite on all four images, with
`task test:int:all`, before each release.

Building migrune with `go install` needs the Go version in
[`go.mod`](go.mod) or later; the images need no Go. migrune follows Go's
[release policy](https://go.dev/doc/devel/release#policy), which supports
the two latest Go releases: `go.mod` stays on one of them, and moves up
before Go stops supporting it.

## Install

With Go:

```sh
go install github.com/joaopaulosr95/migrune/cmd/migrune@latest
```

With Docker:

```sh
docker run --rm ghcr.io/joaopaulosr95/migrune:latest --help
```

Pin an exact version (for example `@v1.0.0` or `:1.0.0`) in CI.

## Usage

Every flag can also be set as an environment variable, `MIGRUNE_<FLAG>`.

A migration file, `migrations/2026-11-05-001-add-archived-flag.json`:

```json
{
  "id": "2026-11-05-001-add-archived-flag",
  "description": "Default archived to false on existing projects",
  "steps": [
    {
      "collection": "projects",
      "operation": "updateMany",
      "filter": { "archived": { "$exists": false } },
      "update": { "$set": { "archived": false } }
    }
  ]
}
```

## Documentation

- [Writing migrations](docs/writing-migrations.md): data migrations, and the
  checks they must pass.
- [Design](docs/design.md): what migrune doesn't guarantee, and why it works
  the way it does.
- Reference: the [CLI](docs/reference/cli/migrune.md),
  [environment variables and exit codes](docs/reference/env-and-exit-codes.md),
  the [migration file](docs/reference/migration-file.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
