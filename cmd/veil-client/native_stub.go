//go:build nativeui && !windows

package main

func openBrowser(string) {}
func pickFile() string  { return "" }
