package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const pluginID = "quota-reset-router"
const pluginVersion = "0.3.0"

type accountLimit struct {
	FiveHourCap   *float64 `yaml:"five_hour_cap" json:"five_hour_cap,omitempty"`
	WeeklyCap     *float64 `yaml:"weekly_cap" json:"weekly_cap,omitempty"`
	ReleaseBefore string   `yaml:"release_before" json:"release_before,omitempty"`
}

type config struct {
	Mode           string                  `yaml:"mode" json:"mode"`
	PollInterval   time.Duration           `yaml:"poll_interval" json:"-"`
	MaxAge         time.Duration           `yaml:"max_age" json:"-"`
	RequestTimeout time.Duration           `yaml:"request_timeout" json:"-"`
	FiveHourCap    float64                 `yaml:"five_hour_cap" json:"five_hour_cap"`
	WeeklyCap      float64                 `yaml:"weekly_cap" json:"weekly_cap"`
	ReleaseBefore  time.Duration           `yaml:"release_before" json:"-"`
	SessionTTL     time.Duration           `yaml:"session_ttl" json:"-"`
	Accounts       map[string]accountLimit `yaml:"accounts" json:"accounts,omitempty"`
}

func decodeConfig(raw []byte) (config, error) {
	cfg := config{
		Mode: "shadow", PollInterval: 5 * time.Minute, MaxAge: 10 * time.Minute, RequestTimeout: 10 * time.Second,
		FiveHourCap: 100, WeeklyCap: 100, ReleaseBefore: 5 * time.Hour, SessionTTL: time.Hour,
	}
	if len(raw) != 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return config{}, errors.New("invalid plugin configuration")
		}
	}
	if cfg.Mode != "shadow" && cfg.Mode != "active" {
		return config{}, errors.New("mode must be shadow or active")
	}
	if cfg.PollInterval < time.Minute || cfg.PollInterval > time.Hour {
		return config{}, errors.New("poll_interval must be between 1m and 1h")
	}
	if cfg.MaxAge < cfg.PollInterval || cfg.MaxAge > time.Hour {
		return config{}, errors.New("max_age must be between poll_interval and 1h")
	}
	if cfg.RequestTimeout < time.Second || cfg.RequestTimeout > 30*time.Second {
		return config{}, errors.New("request_timeout must be between 1s and 30s")
	}
	if err := validPercent("five_hour_cap", cfg.FiveHourCap); err != nil {
		return config{}, err
	}
	if err := validPercent("weekly_cap", cfg.WeeklyCap); err != nil {
		return config{}, err
	}
	if cfg.ReleaseBefore < 0 || cfg.ReleaseBefore > 48*time.Hour {
		return config{}, errors.New("release_before must be between 0s and 48h")
	}
	if cfg.SessionTTL < 0 || cfg.SessionTTL > 24*time.Hour {
		return config{}, errors.New("session_ttl must be between 0s and 24h")
	}
	for id, limit := range cfg.Accounts {
		if strings.TrimSpace(id) == "" {
			return config{}, errors.New("account id is empty")
		}
		if limit.FiveHourCap != nil {
			if err := validPercent("five_hour_cap", *limit.FiveHourCap); err != nil {
				return config{}, err
			}
		}
		if limit.WeeklyCap != nil {
			if err := validPercent("weekly_cap", *limit.WeeklyCap); err != nil {
				return config{}, err
			}
		}
		if strings.TrimSpace(limit.ReleaseBefore) != "" {
			window, err := time.ParseDuration(strings.TrimSpace(limit.ReleaseBefore))
			if err != nil || window < 0 || window > 48*time.Hour {
				return config{}, fmt.Errorf("release_before for %s must be between 0s and 48h", id)
			}
		}
	}
	return cfg, nil
}

func validPercent(name string, value float64) error {
	if value < 0 || value > 100 {
		return fmt.Errorf("%s must be between 0 and 100", name)
	}
	return nil
}

func (c config) fiveHourCap(id string) float64 {
	if limit, ok := c.Accounts[id]; ok && limit.FiveHourCap != nil {
		return *limit.FiveHourCap
	}
	return c.FiveHourCap
}

func (c config) weeklyCap(id string) float64 {
	if limit, ok := c.Accounts[id]; ok && limit.WeeklyCap != nil {
		return *limit.WeeklyCap
	}
	return c.WeeklyCap
}

func (c config) releaseBefore(id string) time.Duration {
	if limit, ok := c.Accounts[id]; ok && strings.TrimSpace(limit.ReleaseBefore) != "" {
		window, err := time.ParseDuration(strings.TrimSpace(limit.ReleaseBefore))
		if err == nil {
			return window
		}
	}
	return c.ReleaseBefore
}
