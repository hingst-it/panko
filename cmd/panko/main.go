package main

import (
	"context"
	"github.com/hingst-it/panko/internal/cli"
	"os"
)

func main() { os.Exit((&cli.App{}).Run(context.Background(), os.Args[1:])) }
