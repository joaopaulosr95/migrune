package cmd

import (
	"runtime/debug"
	"strings"
)

// version is stamped at build time with
// -ldflags "-X github.com/joaopaulosr95/migrune/internal/cmd.version=X.Y.Z".
var version = ""

// Version returns the stamped version, falling back to the module version
// recorded by `go install ...@vX.Y.Z`, or "dev". A leading "v" is stripped,
// so both print X.Y.Z.
func Version() string {
	module := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		module = bi.Main.Version
	}
	return resolveVersion(version, module)
}

func resolveVersion(stamped, module string) string {
	v := stamped
	if v == "" && module != "(devel)" {
		v = module
	}
	if v == "" {
		return "dev"
	}
	return strings.TrimPrefix(v, "v")
}
