package main

import (
	"os"

	"mai/internal/mai"
)

func main() {
	os.Exit(mai.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
