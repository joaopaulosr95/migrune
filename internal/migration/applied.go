package migration

import (
	"fmt"
	"time"
)

// Unknown stands for a runner or an applied time a changelog entry doesn't
// record. migrune always writes both; only an entry written by hand can lack
// them.
const Unknown = "unknown"

// Applied is what the latest Finished changelog entry of a migration records,
// as read by plan, apply and online lint. It is shared by the runner, which
// reads it, and lint, which must not depend on the database.
type Applied struct {
	ID string `json:"id"`
	// Checksum is the checksum of the file as applied.
	Checksum    string `json:"-"`
	Description string `json:"description"`
	// AppliedAt is when the entry was written (its createdAt). Zero when
	// the entry has none.
	AppliedAt time.Time `json:"appliedAt,omitzero"`
	// Runner is empty when the entry has none.
	Runner string `json:"runner,omitempty"`
}

// AppliedAtText returns AppliedAt as RFC 3339 in UTC, or Unknown.
func (a Applied) AppliedAtText() string {
	if a.AppliedAt.IsZero() {
		return Unknown
	}
	return a.AppliedAt.UTC().Format(time.RFC3339)
}

// RunnerText returns Runner, or Unknown.
func (a Applied) RunnerText() string {
	if a.Runner == "" {
		return Unknown
	}
	return a.Runner
}

// MissingContext describes an applied migration whose file is gone, on one
// line: when and by whom it was applied, its description, and where the full
// migration is stored.
func (a Applied) MissingContext() string {
	return fmt.Sprintf("Finished %s by %s: %q; the file is not in --dir. "+
		"The full migration is stored in migrations_changelog (migrationId: %q)",
		a.AppliedAtText(), a.RunnerText(), a.Description, a.ID)
}
