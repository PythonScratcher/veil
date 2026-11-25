//go:build veiltest

package main

import "strings"

func init() {
	dialHook = func(a string) string { return strings.Replace(a, "10.99.0.9:", "127.0.0.1:", 1) }
}
