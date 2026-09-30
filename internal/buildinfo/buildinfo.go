// Package buildinfo carries the version stamped in at build time:
//
//	go build -ldflags "-X github.com/conduit-sync/conduit/internal/buildinfo.Version=0.1.0 ..."
package buildinfo

var (
	Version = "dev"
	Commit  = ""
)

const (
	Author   = "MorphEdit"
	Homepage = "https://github.com/MorphEdit/conduit"
)

// String is the one-line identification shown by `conduit version` and logs.
func String() string {
	s := "Conduit " + Version
	if Commit != "" {
		s += " (" + Commit + ")"
	}
	return s + " - by " + Author + " | " + Homepage
}
