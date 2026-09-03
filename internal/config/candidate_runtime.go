package config

import "context"

// ValidateFunc checks whether a validated candidate can construct runtime state.
type ValidateFunc func(context.Context, *Config, []string) error

// RuntimeConfig returns the expanded candidate used for side-effect-free
// runtime construction after Store validation succeeds.
func (c *Candidate) RuntimeConfig() *Config {
	return c.config
}
