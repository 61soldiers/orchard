//go:build !unix

package main

// watchParent is a no-op where there is no reparenting to watch for.
func watchParent() {}
