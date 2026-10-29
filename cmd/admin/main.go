// Command admin is the legacy panko-admin binary; the same commands are available as panko admin.
package main

import (
	"fmt"
	"os"

	"github.com/hingst-it/panko/internal/admincmd"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("panko-admin " + version)
		return
	}
	if err := admincmd.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
