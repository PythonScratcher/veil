//go:build !windows && !darwin

package main

import "fmt"

const tunName = "veil0"

// On non-Windows platforms configure the interface and routes yourself
// (ip addr/ip route); this is only here so the stack can be developed on Linux.
func configureRoutes(ifname string, cfg *Config) (func(), error) {
	fmt.Printf("interface %s is up; add addresses/routes manually (see README)\n", ifname)
	return func() {}, nil
}

func cleanupStale(cfg *Config) {}
