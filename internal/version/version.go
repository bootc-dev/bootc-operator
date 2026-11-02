// SPDX-License-Identifier: Apache-2.0

// Package version holds build metadata set via linker flags.
package version

// dirtyCommit marks builds from a worktree with uncommitted changes.
const dirtyCommit = "dirty"

var (
	Version   = "dev"
	GitCommit = ""
)

// String returns the version and its Git revision when available.
func String() string {
	switch GitCommit {
	case "":
		return Version
	case dirtyCommit:
		return Version + " (" + dirtyCommit + ")"
	}
	return Version + " (commit " + GitCommit + ")"
}
