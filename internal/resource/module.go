package resource

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var modulePathPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ModulePath checks the module's directory inside its repository: empty for the root, otherwise a
// clean relative path that cannot leave the checkout.
func ModulePath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > 200 || !modulePathPattern.MatchString(p) || path.Clean(p) != p {
		return fmt.Errorf("module path must be a clean relative path inside the module repository")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || part == ".git" {
			return fmt.Errorf("module path must be a clean relative path inside the module repository")
		}
	}
	return nil
}
