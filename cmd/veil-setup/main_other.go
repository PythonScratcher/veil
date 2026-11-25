//go:build !windows

package main

import "fmt"

func main() { fmt.Println("veil-setup is the Windows installer; build it with GOOS=windows") }
