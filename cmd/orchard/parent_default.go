//go:build !android

package main

// watchParent is a no-op off Android: a container or service manager owns the
// process there, and Orchard is expected to outlive whoever launched it.
func watchParent() {}
