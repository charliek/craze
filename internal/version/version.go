// Package version provides the application version, set at build time via ldflags.
package version

// Version defaults to the last released version. That default is bumped
// only by scripts/release/update-version.sh, as part of cutting a release
// (see RELEASING.md) — never edit the literal below by hand.
//
// Two things override the default at build time:
//
//   - `make build` defaults it to "dev" via ldflags
//     (-X github.com/charliek/craze/internal/version.Version=$(VERSION), and
//     the Makefile's VERSION ?= dev is overridable: VERSION=1.2.3 make build).
//   - GoReleaser sets it to the tag being released.
//
// The value below stays "dev" until the first release bumps it to
// "0.0.1" — that bump is also how release.yml's version-check job proves
// the release skill ran before the tag was pushed.
var Version = "0.0.1"
