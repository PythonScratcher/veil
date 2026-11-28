//go:build veiltest

package main

import (
	"os"
	"strings"
)

func init() {
	dialHook = func(a string) string {
		if os.Getenv("VEIL_TEST_NO_PUBLIC") != "" && !strings.HasPrefix(a, "10.99.0.9:") {
			// CI runs server and client on one machine: dialing out would loop back into the
			// client's tunnel, so every outbound connection is refused instead.
			return "127.0.0.1:9"
		}
		return strings.Replace(a, "10.99.0.9:", "127.0.0.1:", 1)
	}
}
