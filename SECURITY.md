# Security

## Supported versions

Only the latest release is supported. Security fixes ship in the next
release; nothing is backported. See [compatibility](docs/reference/compatibility.md).

## Reporting a vulnerability

Report it privately, through
[GitHub's private vulnerability reporting](https://github.com/joaopaulosr95/migrune/security/advisories/new),
not in a public issue. Include the migrune version, what an attacker can do,
and how to reproduce it. Never include a real connection string, password,
token or data.

You'll get a first answer within 7 days. Once a fix is released, the
advisory is published with it and credits you, unless you'd rather not.

migrune runs with database credentials, so issues that leak them (a
connection string or password in logs, reports or errors), leak document
values without `--sample-values`, or let a migration run without the checks
it claims are all in scope.
