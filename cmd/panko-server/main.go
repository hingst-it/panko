// Command panko-server is the shared binary of Panko's runtime roles: gateway, controller (with the
// embedded Metacontroller), endpoint, mirror, runner, agent and setup.
package main

import (
	"os"

	"github.com/hingst-it/panko/internal/roles"
)

var version = "dev"

func main() { os.Exit(roles.Main(os.Args[1:], version)) }
