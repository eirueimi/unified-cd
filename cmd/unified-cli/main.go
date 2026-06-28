package main

import (
	"os"

	"github.com/unified-cd/unified-cd/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
