package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadFakepay(t *testing.T) {
	c, err := LoadFakepay(lookupFrom(map[string]string{"WEBHOOK_SECRET": strings.Repeat("w", 32)}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DelayMin != 100*time.Millisecond || c.DelayMax != 2*time.Second || c.LateExtra != 11*time.Minute ||
		c.DuplicateRate != 0.2 || c.LateRate != 0.05 || c.FailRate != 0.1 || c.PublicURL != "http://localhost:8090" ||
		c.CallbackPrefix != "http://localhost:8080" {
		t.Errorf("defaults = %+v", c)
	}

	for name, env := range map[string]map[string]string{
		"missing secret":  {},
		"rate above one":  {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_5XX_RATE": "1.5"},
		"negative delay":  {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_DELAY_MS_MIN": "-1"},
		"min above max":   {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_DELAY_MS_MIN": "3000"},
		"rate not number": {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_LATE_RATE": "often"},
		"rate NaN":        {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_DUPLICATE_RATE": "NaN"},
		"rate Inf":        {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_DUPLICATE_RATE": "+Inf"},
		"delay overflows": {"WEBHOOK_SECRET": strings.Repeat("w", 32), "FAKEPAY_LATE_EXTRA_MS": "9223372036854775"},
		"bad callback":    {"WEBHOOK_SECRET": strings.Repeat("w", 32), "PUBLIC_BASE_URL": "ftp://x"},
	} {
		var ve *ValidationError
		if _, err := LoadFakepay(lookupFrom(env)); !errors.As(err, &ve) {
			t.Errorf("%s: error = %v, want ValidationError", name, err)
		}
	}
}
