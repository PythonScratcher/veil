//go:build !windows && !darwin

package main

// No tray off Windows (development builds): just wait for quit.
func runTray(g *gui) { <-g.quit }
