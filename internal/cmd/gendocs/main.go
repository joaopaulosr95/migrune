// Command gendocs writes the migrune CLI reference as Markdown. It is a
// separate program so the generator doesn't ship in the migrune binary.
//
// Usage: go run ./internal/cmd/gendocs <output dir>
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra/doc"

	"github.com/joaopaulosr95/migrune/internal/cmd"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gendocs <output dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	root := cmd.NewRootCmd()
	root.DisableAutoGenTag = true
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := doc.GenMarkdownTree(root, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
