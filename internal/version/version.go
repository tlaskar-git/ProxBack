// Package version holds the ProxBack release version baked into every binary.
package version

// Version is the semantic version of this build. Release builds may override it
// with: go build -ldflags "-X proxback/internal/version.Version=1.2.3"
var Version = "0.3.1"
