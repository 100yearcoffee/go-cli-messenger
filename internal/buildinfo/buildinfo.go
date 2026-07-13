package buildinfo

import "fmt"

// These variables can be overridden at build time with -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
)

func String(program string) string {
	return fmt.Sprintf("%s %s (%s)", program, Version, Commit)
}
