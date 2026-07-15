// Package syncer maintains local sparse media stubs and synchronizes media and sidecars.
package syncer

import (
	"errors"
	"log"
	"path/filepath"
	"time"

	"github.com/qbisi/mediastub/core"
	"github.com/qbisi/mediastub/origin"
	"github.com/qbisi/mediastub/pathfilter"
)

// Config controls one synchronization service.
type Config struct {
	Remote       string
	LocalRoot    string
	StateDir     string
	Includes     []string
	PollInterval time.Duration
	SettleTime   time.Duration
	LogLevel     string
	Daemon       bool
	Budget       core.Budget
	Logger       *log.Logger
}

func (c *Config) normalize() error {
	if !filepath.IsAbs(c.LocalRoot) {
		return errors.New("local directory must be absolute")
	}
	if !filepath.IsAbs(c.StateDir) {
		return errors.New("state directory must be absolute")
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Minute
	}
	if c.SettleTime <= 0 {
		c.SettleTime = 3 * time.Second
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogLevel != "info" && c.LogLevel != "verbose" && c.LogLevel != "debug" {
		return errors.New("log level must be info, verbose or debug")
	}
	if c.Includes == nil {
		c.Includes = pathfilter.ParseCommaSeparated(pathfilter.DefaultIncludes)
	}
	if c.Budget.MaxBytes == 0 {
		c.Budget = core.DefaultBudget
	}
	if c.Logger == nil {
		c.Logger = log.Default()
	}
	return nil
}

// New constructs a synchronization service.
func New(upstream origin.Origin, config Config) (*Service, error) {
	if upstream == nil {
		return nil, errors.New("origin is required")
	}
	if _, ok := upstream.(origin.MutableOrigin); !ok {
		return nil, errors.New("origin does not support media and sidecar PUT")
	}
	if err := config.normalize(); err != nil {
		return nil, err
	}
	matcher, err := pathfilter.New(config.Includes)
	if err != nil {
		return nil, err
	}
	return &Service{origin: upstream, mutable: upstream.(origin.MutableOrigin), config: config, matcher: matcher}, nil
}
