// Package version holds the build-time version string for all EntroQ binaries.
// The Version variable is set at link time via:
//
//	go build -ldflags "-X github.com/shiblon/entroq/pkg/version.Version=1.2.3" ...
//
// When built without ldflags (e.g. go run or local dev builds), it defaults to "dev".
package version

// Version is the release version of this binary. Set at build time via ldflags;
// defaults to "dev" for local and development builds.
var Version = "dev"
