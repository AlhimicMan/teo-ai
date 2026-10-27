package main

import (
	"os"
	"teo-ai/internal/launcher"
)

func main() { os.Exit(launcher.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
