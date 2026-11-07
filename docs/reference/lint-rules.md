# Lint rules and expected indexes

`migrune lint --dir <dir>` checks every `*.json` file in the migrations
directory, offline. Subdirectories are not read; lint warns about them
(`ignored-entry`), as plan and apply do in their logs.

With `--uri` and `--db`, lint also reads the target database's changelog,
read-only, and runs the `checksum` and `applied-missing` rules. `--uri` without `--db` is a usage
error; `--db` alone keeps lint offline.

## Output

Each finding is printed to stdout on its own line:

```txt
<file>: <severity>: <rule>: <message>
```

`severity` is `error` or `warning`. Messages about a place inside a file start
with a JSON pointer to it, such as `/steps/0/update/2`. Findings are sorted
by file. Logs, including the final summary, go to stderr.

With `--output json` (`-o json`, `MIGRUNE_OUTPUT=json`), stdout is one JSON
array instead, `[]` when there are no findings, with an object per finding:

```json
[
  {
    "file": "migrations/2026-11-01-001-facet-out.json",
    "severity": "error",
    "rule": "nested-out-merge",
    "message": "/steps/0/update/0/$facet/x/0: $out is not allowed inside a $facet pipeline"
  }
]
```

`--output` is shared with plan and apply, whose default is `table`: lint
reads `table` as `text`, so one `MIGRUNE_OUTPUT` works for every command.

`lint` exits with status 1 if there is at least one error. Warnings alone
exit 0. A usage or configuration problem, such as a missing `--dir` or an
unreadable `--expected-indexes` file, exits 2 (see
[exit codes](env-and-exit-codes.md#exit-codes)).

## Rules

| Rule                  | Severity | Fails when                                                                                                     |
| --------------------- | -------- | -------------------------------------------------------------------------------------------------------------- |
| `file-name`           | error    | the file name isn't `YYYY-MM-DD-NNN-slug.json`, or its date isn't a real date                                  |
| `ignored-entry`       | warning  | an entry of the directory is a subdirectory, or a file with an extension close to `.json` (`.JSON` or another case, `.json5`, `.jsonc`): lint, plan and apply don't read it, so a migration there would never run. plan and apply log the same warning. Other files, such as a `README.md`, are ignored silently |
| `regular-file`        | error    | the file is a symbolic link, or anything else but a regular file. lint doesn't read it; plan and apply refuse it too |
| `file-size`           | error    | the file is over 1 MiB (1,048,576 bytes). lint doesn't read it; plan and apply refuse it too                   |
| `line-endings`        | error    | the file has a carriage return (CRLF line endings), or starts with a UTF-8 byte order mark. The checksum covers the raw bytes, so a checkout or an editor that changes them makes the migration look edited wherever it is applied. A file with a byte order mark doesn't load either, in plan and apply. Use LF and no BOM, and add `migrations/*.json -text` to `.gitattributes` |
| `json`                | error    | the file isn't valid JSON                                                                                      |
| `duplicate-key`       | error    | an object has the same key twice, such as two `collection` in a step: JSON parsers disagree on which one wins. The message gives the JSON pointer of the second one. plan and apply refuse the file too |
| `extended-json`       | error    | a value isn't valid MongoDB Extended JSON, such as a malformed `$oid`                                          |
| `schema`              | error    | the file doesn't match the [migration file schema](migration-file.md#json-schema), which owns what it can express: the fields of each step, the allowed operations (no `insert*`: seed data with `updateOne`, `upsert: true` and `$setOnInsert`), one kind of migration per file, and no `$out` or `$merge` stage in an update pipeline. Each problem is one finding, with a message that says what to do |
| `id`                  | error    | the `id` field differs from the file name without `.json`                                                      |
| `duplicate-prefix`    | error    | another file has the same `YYYY-MM-DD-NNN` prefix                                                              |
| `empty-filter`        | warning  | a `deleteMany` or `updateMany` has the filter `{}`, which matches every document in its collection. Make sure that is meant, and check the counts plan reports |
| `nested-out-merge`    | error    | `$out` or `$merge` appears in a nested pipeline: `$lookup.pipeline`, `$unionWith.pipeline` or a `$facet` output, at any depth |
| `server-js`           | error    | a step uses `$where`, `$function` or `$accumulator`, at any depth: they run JavaScript on the server, which migrune can't check, which is slow, often disabled, and deprecated from MongoDB 8.0 |
| `reserved-collection` | error    | a pipeline reads or writes a [reserved collection](migration-file.md#reserved-collections) (`migrations_changelog`, `migrations_lock` or `system.*`): the `from` of `$lookup` or `$graphLookup`, the collection of `$unionWith`, or the target of `$out`, at any depth. A reserved step `collection` is a `schema` finding. plan and apply refuse the file too |
| `index-name-reused`   | error    | during the [index replay](#index-replay), `createIndex` reuses a live index name in the same collection for a different definition |
| `index-conflict`      | error    | during the [index replay](#index-replay), `createIndex` uses the key pattern of a live index with another name in the same collection, and its `collation`, `partialFilterExpression`, `unique`, `sparse` and `wildcardProjection` are the same |
| `drop-then-create`    | error    | during the [index replay](#index-replay), a migration drops an index and later creates one with the same name or keys in the same collection |
| `ttl-index`           | warning  | a `createIndex` sets `expireAfterSeconds`: the TTL index deletes every document whose indexed date is older than that as soon as it is built. plan reports how many in `wouldExpire` |
| `index-missing`       | error    | an index in the [`--expected-indexes`](#expected-indexes) file has no migration                                                     |
| `index-options`       | error    | a migrated index has the expected keys but different options (see [how indexes are matched](#how-indexes-are-matched)) |
| `index-extra`         | warning  | a migrated index isn't in the `--expected-indexes` file                                                        |
| `index-renamed`       | warning  | a migrated index matches an expected one by definition but has another name                                   |
| `checksum`            | error    | online only: the migration is `Finished` in the target database and its file changed since; edit a migration with only `Errored` entries instead, or write a new one |
| `applied-missing`     | error    | online only: a migration is `Finished` in the target database but its file isn't in the directory; plan and apply fail too. The finding is on the file it would have, `<dir>/<id>.json`, and gives what its latest `Finished` entry stored: when and by which runner it was applied, and its description. To retire a migration on purpose, delete its changelog entries by hand |

An `applied-missing` finding looks like this, on one line:

```txt
migrations/2026-10-01-001-gone.json: error: applied-missing: Finished 2026-01-04T10:12:00Z by ci-7: "Backfill the flag"; the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "2026-10-01-001-gone"). plan and apply fail until the file is restored: removing a file doesn't undo its migration; to retire a migration on purpose, delete its changelog entries by hand
```

To fix it, see [troubleshooting](../troubleshooting.md#applied-but-missing).

Each problem is reported once: the rules above check only what the schema
can't express. A step whose operation is unknown, or of another kind than
the file's first step, gets one `schema` finding on its `operation`, not one
per field the operation doesn't take. A bad `id` gets one finding: `file-name`
when the file name is bad too, otherwise `id` when it differs from the file
name, and only then `schema` for breaking the id pattern.

## Index replay

lint replays every `createIndex` and `dropIndex` step of the index migrations
in id order, without a database, with or without `--expected-indexes`. It
fails when MongoDB would reject a `createIndex` once the migration is
applied:

- `index-name-reused`: it reuses the name of a live index in the same
  collection for a different definition. Pick another name, and drop the old
  index in a later migration if nothing needs it.
- `index-conflict`: a live index in the same collection, under another name,
  has the same keys in the same order, and the two indexes have the same
  `collation`, `partialFilterExpression`, `unique`, `sparse` and
  `wildcardProjection`. This covers the same definition under a new name and
  the same keys with any other option different, such as
  `expireAfterSeconds`, `hidden` or a 2d index's `bits`. MongoDB lets indexes
  on the same keys coexist only when one of those five options differs. Keep using the old index, or drop it in an earlier migration.

- `drop-then-create`: a migration drops an index, then creates one in the
  same collection with its name or its keys. If the build fails, such as a
  unique index on duplicates, the collection is left without either. Create
  the new index under a new name first, and drop the old one in a later
  migration. The keys of a dropped index are known when an earlier
  migration created it; otherwise only the name is compared.

An index dropped earlier in the replay doesn't count as live. A rejected
`createIndex` doesn't create anything, so later steps and the comparison with
the expected-indexes file see the index that was already there.

## Expected indexes

`--expected-indexes <file>` compares the indexes your migrations create with
the indexes your application expects. The application side comes from this
file, which each project generates with its own tooling (for example, a script
that dumps the indexes declared in its models). migrune never reads
application code, so the check works with any language.

The file is a JSON array with one object per index. Values are
[MongoDB Extended JSON](https://www.mongodb.com/docs/manual/reference/mongodb-extended-json/),
relaxed or canonical. There is no version field.

```json
[
  {
    "collection": "accounts",
    "name": "name_1_ci",
    "keys": { "name": 1 },
    "options": { "unique": true, "collation": { "locale": "en", "strength": 2 } }
  },
  {
    "collection": "sessions",
    "name": "expiresAt_1",
    "keys": { "expiresAt": 1 },
    "options": { "expireAfterSeconds": 3600 }
  },
  {
    "collection": "articles",
    "keys": { "title": "text", "body": "text" },
    "options": { "weights": { "title": 10 }, "default_language": "english" }
  }
]
```

| Field        | Required | Description                                                              |
| ------------ | -------- | ------------------------------------------------------------------------ |
| `collection` | yes      | Collection name, not empty.                                              |
| `name`       | no       | Index name. Used only to warn about renames; never used for matching.    |
| `keys`       | yes      | Index keys, not empty. Their order is kept and is part of the index.     |
| `options`    | no       | Index options as passed to `createIndex`, or as `listIndexes` shows them. |

Any other field is an error. A file that can't be read or parsed makes
`lint` exit with status 2.

### How indexes are matched

The indexes left at the end of the [index replay](#index-replay) are compared
with the file.

Two indexes are the same when they have the same **definition**:

- the same collection;
- the same keys in the same order (`{ "a": 1, "b": 1 }` differs from
  `{ "b": 1, "a": 1 }`); numbers compare by value, so `1`, `1.0` and
  `{ "$numberLong": "1" }` are equal;
- the same **options**: every option but `background`, `textIndexVersion`,
  `2dsphereIndexVersion` and `prepareUnique` (and `name`, `v`, `ns` and
  `key`, which aren't options). MongoDB ignores the first three when it
  compares an index with an existing one; `prepareUnique` is a step on the
  way to `unique` that an application doesn't declare. So
  `wildcardProjection`, a 2d index's `bits`, `min` and `max`,
  `storageEngine` and any other option count as much as `unique` or
  `expireAfterSeconds`.

The options compare as MongoDB compares them:

- A `false` boolean is the same as leaving it out, and so is a collation of
  just `{ "locale": "simple" }`.
- A collation as `listIndexes` shows it, which has every default filled in
  and a `version`, a field `createIndex` doesn't take, compares as a
  subset: every field the migration gives must have the same value in it, and
  the others count as defaults. So a non-default field the migration never
  set, such as `caseLevel: true`, goes unnoticed. Any other collation compares
  field for field, so give one as passed to `createIndex` to compare it
  exactly.
- Fields inside an option that is a document, such as
  `partialFilterExpression`, `collation` or `weights`, may come in any order.
- A `wildcardProjection` may be nested or dotted (`{ "a": { "b": 1 } }` is
  `{ "a.b": 1 }`), with any true value for 1 and any false one for 0, and
  `"_id": 0`, the default, may be left out.
- A text index is compared as `listIndexes` lists it: its text fields count
  as keys `{ "_fts": "text", "_ftsx": 1 }` and as `weights`, 1 unless
  `weights` says otherwise; `default_language` is `"english"` and
  `language_override` is `"language"` unless given. MongoDB fills these in,
  and rejects a `createIndex` that differs from them.
- Other defaults aren't filled in, as MongoDB doesn't: a 2d index with
  `"bits": 26`, its default, differs from one without `bits`.

The `_id_` index is skipped on both sides.

| Outcome                                                              | Severity | Rule            |
| -------------------------------------------------------------------- | -------- | --------------- |
| An expected index that no migration creates                          | error    | `index-missing` |
| A migrated index with the expected keys but other options            | error    | `index-options` |
| A migrated index that the file doesn't list                          | warning  | `index-extra`   |
| A migrated index matching an expected one under another name         | warning  | `index-renamed` |
