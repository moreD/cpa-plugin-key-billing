package billing

import "testing"

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, errDecode := DecodeConfig([]byte("enabled: true\npriority: 10\nstore:\n  id: cpa-key-billing\n  version: 0.5.1\n"))
	if errDecode != nil {
		t.Fatalf("DecodeConfig: %v", errDecode)
	}
	if !cfg.Enabled || cfg.Debug || cfg.CodexFastModeBilling || cfg.MaskAPIKeyViewEmails || cfg.AllowAPIKeyQuotaReset || cfg.StateFile != DefaultStateFile || cfg.SchedulerMode != "disabled" {
		t.Fatalf("config = %+v", cfg)
	}
	cfg, errDecode = DecodeConfig([]byte("enabled: true\ndebug: true\ncodex_fast_mode_billing: true\nmask_api_key_view_emails: true\nallow_api_key_quota_reset: true\n"))
	if errDecode != nil || !cfg.Debug || !cfg.CodexFastModeBilling || !cfg.MaskAPIKeyViewEmails || !cfg.AllowAPIKeyQuotaReset {
		t.Fatalf("config = %+v, error = %v", cfg, errDecode)
	}
}

func TestDecodeConfigNormalizesSchedulerMode(t *testing.T) {
	for raw, want := range map[string]string{
		"scheduler_mode: smart\n":    "smart",
		"scheduler_mode: REGULAR\n":  "regular",
		"scheduler_mode: disabled\n": "disabled",
		"scheduler_mode: unknown\n":  "disabled",
		"scheduler_mode: \"\"\n":     "disabled",
	} {
		cfg, err := DecodeConfig([]byte(raw))
		if err != nil {
			t.Fatalf("DecodeConfig(%q): %v", raw, err)
		}
		if cfg.SchedulerMode != want {
			t.Fatalf("DecodeConfig(%q).SchedulerMode = %q, want %q", raw, cfg.SchedulerMode, want)
		}
	}
}

func TestDecodeConfigRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field":  "enable: true\n",
		"extra document": "enabled: true\n---\nenabled: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, errDecode := DecodeConfig([]byte(raw)); errDecode == nil {
				t.Fatal("DecodeConfig accepted invalid configuration")
			}
		})
	}
}
