package billing

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultStateFile = "plugins/cpa-key-billing-state-v1.db"

type Config struct {
	Enabled               bool   `yaml:"enabled"`
	Debug                 bool   `yaml:"debug"`
	StateFile             string `yaml:"state_file"`
	SchedulerMode         string `yaml:"scheduler_mode"`
	SmartSticky           bool   `yaml:"smart_sticky"`
	SmartStickyTTLSeconds int    `yaml:"smart_sticky_ttl_seconds"`
	SmartWindowSeconds    int    `yaml:"smart_window_seconds"`
	SmartMaxInflight      int    `yaml:"smart_max_inflight_per_profile"`
	CodexFastModeBilling  bool   `yaml:"codex_fast_mode_billing"`
	MaskAPIKeyViewEmails  bool   `yaml:"mask_api_key_view_emails"`
	AllowAPIKeyQuotaReset bool   `yaml:"allow_api_key_quota_reset"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:               false,
		StateFile:             DefaultStateFile,
		SchedulerMode:         "disabled",
		SmartSticky:           true,
		SmartStickyTTLSeconds: 24 * 60 * 60,
		SmartWindowSeconds:    120,
		SmartMaxInflight:      8,
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(bytes.TrimSpace(raw)) > 0 {
		document := struct {
			Config `yaml:",inline"`
			// These fields belong to the host and are ignored by the plugin.
			Priority int       `yaml:"priority"`
			Store    yaml.Node `yaml:"store"`
		}{Config: cfg}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&document); errDecode != nil {
			return Config{}, fmt.Errorf("Parse plugin configuration: %w", errDecode)
		}
		if errTrailing := decoder.Decode(&struct{}{}); errTrailing != io.EOF {
			return Config{}, fmt.Errorf("Plugin configuration must contain exactly one YAML document")
		}
		cfg = document.Config
	}
	return cfg.normalized(), nil
}

func (c Config) describe() string {
	if c.Enabled {
		return "enabled"
	}
	return "disabled"
}

func (c Config) normalized() Config {
	c.StateFile = strings.TrimSpace(c.StateFile)
	if c.StateFile == "" {
		c.StateFile = DefaultStateFile
	}
	c.SchedulerMode = strings.ToLower(strings.TrimSpace(c.SchedulerMode))
	if c.SchedulerMode == "" {
		c.SchedulerMode = "disabled"
	}
	switch c.SchedulerMode {
	case "disabled", "regular", "smart":
	default:
		c.SchedulerMode = "disabled"
	}
	if c.SmartStickyTTLSeconds <= 0 {
		c.SmartStickyTTLSeconds = 24 * 60 * 60
	}
	if c.SmartWindowSeconds <= 0 {
		c.SmartWindowSeconds = 120
	}
	if c.SmartWindowSeconds > 3600 {
		c.SmartWindowSeconds = 3600
	}
	if c.SmartMaxInflight <= 0 {
		c.SmartMaxInflight = 8
	}
	if c.SmartMaxInflight > 10000 {
		c.SmartMaxInflight = 10000
	}
	return c
}
