package main

import (
	"os"

	"github.com/eirueimi/unified-cd/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		os.Exit(1)
	}
}
