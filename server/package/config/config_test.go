package config

import (
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SQL_DSN", "host=localhost user=relay password=relay dbname=relay port=5432 sslmode=disable")
	t.Setenv("MONGO_URLS", "mongodb://localhost:27017")
}

func TestNew_DefaultsMatchLocalDaemonNumbers(t *testing.T) {
	setRequiredEnv(t)
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"PORT", c.PORT, 5555},
		{"PairRateLimit", c.PairRateLimit, 20},
		{"SenderRateLimit", c.SenderRateLimit, 60},
		{"RPCRateLimit", c.RPCRateLimit, 200},
		{"RPCRateWindow", c.RPCRateWindow, 10 * time.Second},
		{"MaxInFlightRPCs", c.MaxInFlightRPCs, 16},
		{"DedupWindow", c.DedupWindow, 30 * time.Second},
		{"MessageTTL", c.MessageTTL, time.Hour},
		{"SweepEvery", c.SweepEvery, 30 * time.Second},
		{"DisconnectGrace", c.DisconnectGrace, 15 * time.Minute},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestNew_KnobsAreOverridable(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PAIR_RATE_LIMIT", "5")
	t.Setenv("MESSAGE_TTL", "10m")
	t.Setenv("SWEEP_EVERY", "1s")
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.PairRateLimit != 5 {
		t.Errorf("PairRateLimit = %d, want 5 (an Oracle free-tier VM should be able to tune this down)", c.PairRateLimit)
	}
	if c.MessageTTL != 10*time.Minute {
		t.Errorf("MessageTTL = %v, want 10m", c.MessageTTL)
	}
	if c.SweepEvery != time.Second {
		t.Errorf("SweepEvery = %v, want 1s", c.SweepEvery)
	}
}
