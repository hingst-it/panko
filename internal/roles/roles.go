// Package roles implements the runtime roles of the shared panko-server binary. Each role runs in
// its own pod or Job with its own ServiceAccount; the binary only selects the code path.
package roles

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Role runs one runtime role with the arguments after its name.
type Role func(args []string) error

var Roles = map[string]Role{
	"gateway":    Gateway,
	"controller": Controller,
	"endpoint":   Endpoint,
	"mirror":     Mirror,
	"runner":     Runner,
	"agent":      Agent,
	"setup":      Setup,
}

func Usage() string {
	names := []string{}
	for name := range Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	return "usage: panko-server " + strings.Join(names, "|") + " [flags]"
}

// Main runs the role named by the first argument and returns the process exit code.
func Main(args []string, version string) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		fmt.Println("panko-server " + version)
		return 0
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, Usage())
		return 2
	}
	role, ok := Roles[args[0]]
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown role "+args[0]+"; "+Usage())
		return 2
	}
	if err := role(args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
