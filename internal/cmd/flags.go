package cmd

import "github.com/spf13/pflag"

// connFlags are the flags every command uses to find migrations and the
// target database.
type connFlags struct {
	dir string
	uri string
	db  string
}

func (c *connFlags) register(fs *pflag.FlagSet) {
	fs.StringVar(&c.dir, "dir", "", "directory holding the migration files")
	fs.StringVar(&c.uri, "uri", "", "MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments")
	fs.StringVar(&c.db, "db", "", "target database name")
}
