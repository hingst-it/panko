package main

import (
	"context"
	"fmt"
	"github.com/hingst-it/panko/internal/cli"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("panko " + version)
		return
	}
	os.Exit((&cli.App{}).Run(context.Background(), os.Args[1:]))
}
