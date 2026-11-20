package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const pluginID = "quota-reset-router"
const pluginVersion = "0.4.0"

const maxReserve = 95

// accountReserve keeps part of a window unused. A value R means the account stops
// taking traffic at 100-R percent utilization while another account is available.
type accountReserve struct {
	FiveHourReserve *float64 `yaml:"five_hour_reserve" json:"five_hour_reserve,omitempty"`
	WeeklyReserve   *float64 `yaml:"weekly_reserve" json:"weekly_reserve,omitempty"`
}

type config struct {
	Mode           string                    `yaml:"mode" json:"mode"`
	PollInterval   time.Duration             `yaml:"poll_interval" json:"-"`
	RequestTimeout time.Duration             `yaml:"request_timeout" json:"-"`
	Accounts       map[string]accountReserve `yaml:"accounts" json:"accounts,omitempty"`
}

func decodeConfig(raw []byte) (config, error) {
	cfg := config{Mode: "shadow", PollInterval: 30 * time.Minute, RequestTimeout: 10 * time.Second}
	if len(raw) != 0 {
		// Unknown keys (including the removed caps and session settings) are ignored.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return config{}, errors.New("invalid plugin configuration")
		}
	}
	if cfg.Mode != "shadow" && cfg.Mode != "active" {
		return config{}, errors.New("mode must be shadow or active")
	}
	if cfg.PollInterval < 5*time.Minute || cfg.PollInterval > 6*time.Hour {
		return config{}, errors.New("poll_interval must be between 5m and 6h")
	}
	if cfg.RequestTimeout < time.Second || cfg.RequestTimeout > 30*time.Second {
		return config{}, errors.New("request_timeout must be between 1s and 30s")
	}
	seen := make(map[string]bool, len(cfg.Accounts))
	for id, reserve := range cfg.Accounts {
		key := accountKey(id)
		if key == "" {
			return config{}, errors.New("account id is empty")
		}
		if seen[key] {
			return config{}, fmt.Errorf("account %s is configured more than once", key)
		}
		seen[key] = true
		for name, value := range map[string]*float64{"five_hour_reserve": reserve.FiveHourReserve, "weekly_reserve": reserve.WeeklyReserve} {
			if value != nil && (math.IsNaN(*value) || *value < 0 || *value > maxReserve) {
				return config{}, fmt.Errorf("%s for %s must be between 0 and %d", name, key, maxReserve)
			}
		}
	}
	return cfg, nil
}

func accountKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// reserveFor returns the reserve configured for an account, keyed by auth ID first and
// then by email, both compared case-insensitively.
func (c config) reserveFor(id, email string) accountReserve {
	for _, want := range []string{accountKey(id), accountKey(email)} {
		if want == "" {
			continue
		}
		for key, reserve := range c.Accounts {
			if accountKey(key) == want {
				return reserve
			}
		}
	}
	return accountReserve{}
}
