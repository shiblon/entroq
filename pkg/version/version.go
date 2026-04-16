// Package version holds the build-time version string for all EntroQ binaries.
// The Version variable is set at link time via:
//
//	go build -ldflags "-X github.com/shiblon/entroq/pkg/version.injected=1.2.3" ...
//
// When built without ldflags (e.g. go install with a version tag), the version
// is read from the module's build info embedded by the Go toolchain. Local and
// development builds default to "dev".
package version

import "runtime/debug"

// injected is set at link time via -ldflags. Takes precedence over all other
// version sources.
var injected string

// Version is the release version of this binary. Resolved at init time from
// (in priority order): ldflags injection, Go toolchain build info, "dev".
var Version = func() string {
	if injected != "" {
		return injected
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}()
