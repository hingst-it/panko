package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/hingst-it/panko/internal/jsonx"
)

// Config is the owner's installation choice: which teams exist and whether the lab S3 gateway runs.
type Config struct {
	Teams []string `json:"teams"`
	S3    bool     `json:"s3"`
	// Egress restricts runner pods to Panko's components and EgressTargets; the mirrors become required.
	Egress        bool           `json:"egress,omitempty"`
	EgressTargets []EgressTarget `json:"egressTargets,omitempty"`
}

// Same pattern as Database spec.owner, because the team is the default owner of its resources.
var teamName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// Team names become namespaces; panko-state-<team> must stay a valid namespace name.
func validTeam(name string) error {
	if !teamName.MatchString(name) || len(name) > 40 {
		return fmt.Errorf("team %q must start with a letter, use lowercase letters, digits and single hyphens, and have at most 40 characters", name)
	}
	for _, reserved := range []string{"infra-", "kube-", "panko-", "metacontroller", "default"} {
		if strings.HasPrefix(name, reserved) {
			return fmt.Errorf("team %q uses the reserved prefix %q", name, reserved)
		}
	}
	return nil
}
func (c Config) Validate() error {
	if len(c.Teams) == 0 {
		return fmt.Errorf("at least one team is required")
	}
	for _, target := range c.EgressTargets {
		if err := target.validate(); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, team := range c.Teams {
		if err := validTeam(team); err != nil {
			return err
		}
		if seen[team] {
			return fmt.Errorf("team %q is listed twice", team)
		}
		seen[team] = true
	}
	return nil
}

// LoadConfig reads and validates the configuration and makes its teams the generators' input.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("cannot read configuration %s: %w", path, err)
	}
	var c Config
	if err := jsonx.Decode(bytes.NewReader(data), &c); err != nil {
		return Config{}, fmt.Errorf("invalid configuration %s", path)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	Teams = c.Teams
	return c, nil
}

// AddTeam appends a team to the configuration; existing teams are kept as they are.
func AddTeam(path, team string) error {
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	for _, existing := range c.Teams {
		if existing == team {
			return nil
		}
	}
	c.Teams = append(c.Teams, team)
	if err := c.Validate(); err != nil {
		return err
	}
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0644)
}
