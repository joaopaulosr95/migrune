# Lint rules

`migrune lint --dir <dir>` checks every `*.json` file in the migrations
directory, offline. Subdirectories are not read; lint warns about them
(`ignored-entry`).

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

`lint` exits with status 1 if there is at least one error. Warnings alone
exit 0. A usage or configuration problem, such as a missing `--dir`, exits 2 (see
[exit codes](env-and-exit-codes.md#exit-codes)).

## Rules

| Rule                  | Severity | Fails when                                                                                                     |
| --------------------- | -------- | -------------------------------------------------------------------------------------------------------------- |
| `file-name`           | error    | the file name isn't `YYYY-MM-DD-NNN-slug.json`, or its date isn't a real date                                  |
| `ignored-entry`       | warning  | an entry of the directory is a subdirectory, or a file with an extension close to `.json` (`.JSON` or another case, `.json5`, `.jsonc`): migrune doesn't read it, so a migration there would never run. Other files, such as a `README.md`, are ignored silently |
| `regular-file`        | error    | the file is a symbolic link, or anything else but a regular file. lint doesn't read it |
| `file-size`           | error    | the file is over 1 MiB (1,048,576 bytes). lint doesn't read it                                                 |
| `line-endings`        | error    | the file has a carriage return (CRLF line endings), or starts with a UTF-8 byte order mark. The checksum covers the raw bytes, so a checkout or an editor that changes them makes the migration look edited wherever it is applied. A file with a byte order mark doesn't load either. Use LF and no BOM, and add `migrations/*.json -text` to `.gitattributes` |
| `json`                | error    | the file isn't valid JSON                                                                                      |
| `duplicate-key`       | error    | an object has the same key twice, such as two `collection` in a step: JSON parsers disagree on which one wins. The message gives the JSON pointer of the second one |
| `extended-json`       | error    | a value isn't valid MongoDB Extended JSON, such as a malformed `$oid`                                          |
| `schema`              | error    | the file doesn't match the [migration file schema](migration-file.md#json-schema), which owns what it can express: the fields of each step, the allowed operations (no `insert*`: seed data with `updateOne`, `upsert: true` and `$setOnInsert`), and no `$out` or `$merge` stage in an update pipeline. Each problem is one finding, with a message that says what to do |
| `id`                  | error    | the `id` field differs from the file name without `.json`                                                      |
| `duplicate-prefix`    | error    | another file has the same `YYYY-MM-DD-NNN` prefix                                                              |
| `empty-filter`        | warning  | a `deleteMany` or `updateMany` has the filter `{}`, which matches every document in its collection. Make sure that is meant |
| `nested-out-merge`    | error    | `$out` or `$merge` appears in a nested pipeline: `$lookup.pipeline`, `$unionWith.pipeline` or a `$facet` output, at any depth |
| `server-js`           | error    | a step uses `$where`, `$function` or `$accumulator`, at any depth: they run JavaScript on the server, which migrune can't check, which is slow, often disabled, and deprecated from MongoDB 8.0 |
| `reserved-collection` | error    | a pipeline reads or writes a [reserved collection](migration-file.md#reserved-collections) (`migrations_changelog`, `migrations_lock` or `system.*`): the `from` of `$lookup` or `$graphLookup`, the collection of `$unionWith`, or the target of `$out`, at any depth. A reserved step `collection` is a `schema` finding |
| `checksum`            | error    | online only: the migration is `Finished` in the target database and its file changed since; edit a migration with only `Errored` entries instead, or write a new one |
| `applied-missing`     | error    | online only: a migration is `Finished` in the target database but its file isn't in the directory. The finding is on the file it would have, `<dir>/<id>.json`, and gives what its latest `Finished` entry stored: when and by which runner it was applied, and its description. To retire a migration on purpose, delete its changelog entries by hand |

An `applied-missing` finding looks like this, on one line:

```txt
migrations/2026-10-01-001-gone.json: error: applied-missing: Finished 2026-01-04T10:12:00Z by ci-7: "Backfill the flag"; the file is not in --dir. The full migration is stored in migrations_changelog (migrationId: "2026-10-01-001-gone"). Restore the file: removing a file doesn't undo its migration; to retire a migration on purpose, delete its changelog entries by hand
```

To fix it, see [troubleshooting](../troubleshooting.md#applied-but-missing).

Each problem is reported once: the rules above check only what the schema
can't express. A step whose operation is unknown gets one `schema` finding
on its `operation`, not one per field the operation doesn't take. A bad `id`
gets one finding: `file-name` when the file name is bad too, otherwise `id`
when it differs from the file name, and only then `schema` for breaking the
id pattern.

