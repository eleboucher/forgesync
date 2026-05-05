// Package version exposes build-time version information.
package version

import "runtime/debug"

// Version is overridden at build time via -ldflags "-X .../internal/version.Version=...".
var Version = "dev"

func String() string {
	if Version != "dev" && Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func UserAgent() string {
	return "forgesync/" + String() + " (+https://git.erwanleboucher.dev/eleboucher/forgesync)"
}
