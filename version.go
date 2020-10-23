package main

import (
	"fmt"
)

var (
	// Package package name
	Package = "golinks"

	// Version release version
	Version = "0.0.5"

	// Commit will be overwritten automatically by the build system
	Commit = "HEAD"
)

// FullVersion display the full version and build
func FullVersion() string {
	return fmt.Sprintf("%s-%s@%s", Package, Version, Commit)
}
