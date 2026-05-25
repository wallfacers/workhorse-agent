// Package version holds the build version used in User-Agent and diagnostics.
// The value is derived from runtime/debug.BuildInfo when available, falling
// back to "dev".
package version

import "runtime/debug"

var Version = func() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}()

// UserAgent returns the User-Agent header value sent to upstream LLM providers.
func UserAgent() string {
	return "workhorse-agent/" + Version
}
