//go:build !windows && !darwin

package main

func resetExistingConnections(cfg *Config) int { return 0 }
