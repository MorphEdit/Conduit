// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package buildinfo carries the version stamped in at build time:
//
//	go build -ldflags "-X github.com/MorphEdit/conduit/internal/buildinfo.Version=0.1.0 ..."
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
