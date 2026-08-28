package orchestrator

import (
	"fmt"
	"time"
)

const (
	defaultWorkflowName           = "go_optimizer_campaign"
	defaultMaxCandidates          = 12
	defaultMaxConsecutiveFailures = 4
)

// Config bounds the graph independently of model judgment.
type Config struct {
	WorkflowName           string
	MaxCandidates          int
	MaxConsecutiveFailures int
	DeterministicTimeout   time.Duration
	AgentTimeout           time.Duration
	MaxConcurrency         int
}

// DefaultConfig matches the version-one campaign limits in the plan. The
// overall 90-minute campaign deadline is supplied by the caller's context;
// these are per-node safety limits.
func DefaultConfig() Config {
	return Config{
		WorkflowName:           defaultWorkflowName,
		MaxCandidates:          defaultMaxCandidates,
		MaxConsecutiveFailures: defaultMaxConsecutiveFailures,
		DeterministicTimeout:   20 * time.Minute,
		AgentTimeout:           20 * time.Minute,
		MaxConcurrency:         1,
	}
}

func (c Config) normalized() (Config, error) {
	c = c.withDefaults()
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) withDefaults() Config {
	defaults := DefaultConfig()
	if c.WorkflowName == "" {
		c.WorkflowName = defaults.WorkflowName
	}
	if c.MaxCandidates == 0 {
		c.MaxCandidates = defaults.MaxCandidates
	}
	if c.MaxConsecutiveFailures == 0 {
		c.MaxConsecutiveFailures = defaults.MaxConsecutiveFailures
	}
	if c.DeterministicTimeout == 0 {
		c.DeterministicTimeout = defaults.DeterministicTimeout
	}
	if c.AgentTimeout == 0 {
		c.AgentTimeout = defaults.AgentTimeout
	}
	if c.MaxConcurrency == 0 {
		c.MaxConcurrency = defaults.MaxConcurrency
	}
	return c
}

func (c Config) validate() error {
	if c.MaxCandidates < 1 {
		return fmt.Errorf("max candidates must be positive")
	}
	if c.MaxConsecutiveFailures < 1 {
		return fmt.Errorf("max consecutive failures must be positive")
	}
	if c.DeterministicTimeout < 0 || c.AgentTimeout < 0 {
		return fmt.Errorf("node timeouts cannot be negative")
	}
	if c.MaxConcurrency < 1 {
		return fmt.Errorf("max concurrency must be positive")
	}
	return nil
}
