package config

import (
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("SHIPIT_DOMAIN", "ship.test")
	t.Setenv("SHIPIT_ADMIN_TOKEN", "t")
	t.Setenv("SHIPIT_GITHUB_TOKEN", "t")
	t.Setenv("SHIPIT_MASTER_KEY", strings.Repeat("ab", masterKeySize))
}

func TestLoadDecodesMasterKey(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MasterKey) != masterKeySize || c.MasterKey[0] != 0xab {
		t.Errorf("key = %x", c.MasterKey)
	}
}

func TestLoadRejectsBadMasterKey(t *testing.T) {
	for name, key := range map[string]string{
		"missing":   "",
		"too short": "abcd",
		"not hex":   strings.Repeat("zz", masterKeySize),
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv("SHIPIT_MASTER_KEY", key)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "SHIPIT_MASTER_KEY") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
