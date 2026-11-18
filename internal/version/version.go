// Package version is the build's identity. The release is the root VERSION file, copied here by
// scripts/set-version.sh (scripts/check-version.sh and this package's tests hold them equal); the
// commit comes from the deploy's environment, or from the Go toolchain's VCS stamp on a local build.
package version

import (
	"os"
	"runtime/debug"
	"sync"
	"time"
)

// Version is the release, SemVer MAJOR.MINOR.PATCH with an optional -PRERELEASE (1.0.0-beta.3).
// Edit it with scripts/set-version.sh only.
const Version = "1.0.0"

// builtAt is stamped by the image build: -ldflags "-X .../internal/version.builtAt=<RFC 3339>".
var builtAt string

// Info is what GET /version and `prismaport version` report.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
}

// Get reads the build's identity. The commit is read from the environment on every call, since
// the platform injects it per deploy, not at compile time.
func Get() Info {
	return Info{Version: Version, Commit: commit(), BuiltAt: built()}
}

// Label is the short form the frontends also show: v<version>+<commit>.
func Label() string {
	return "v" + Version + "+" + commit()
}

func commit() string {
	for _, k := range []string{"RAILWAY_GIT_COMMIT_SHA", "GIT_COMMIT"} {
		if v := os.Getenv(k); v != "" {
			return short(v)
		}
	}
	if v := vcsRevision(); v != "" {
		return short(v)
	}
	return "unknown"
}

func built() string {
	if t, err := time.Parse(time.RFC3339, builtAt); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// vcsRevision is the commit `go build` stamps from the checkout. The image build has no .git, so
// there it is empty and the environment is the only source.
var vcsRevision = sync.OnceValue(func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range info.Settings {
			if kv.Key == "vcs.revision" {
				return kv.Value
			}
		}
	}
	return ""
})
