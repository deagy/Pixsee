package main

// AC-4 unit tests: the host refuses to start on non-positive or capped
// heartbeat flags, or when HeartbeatInterval outruns HeartbeatTimeout plus the
// steady-state slack (spec D7), with an error naming both values. Defaults
// (30/30/10) start cleanly.

import (
	"strings"
	"testing"
	"time"

	"virtualdesktop/internal/transport"
)

func TestLoadConfigHeartbeatValidation(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantErr     bool
		wantErrHas  []string // substrings the startup error must name
		wantInt     time.Duration
		wantTimeout time.Duration
		wantIO      time.Duration
	}{
		{
			name:        "defaults start cleanly (30/30/10)",
			args:        []string{"-addr", "127.0.0.1:0"},
			wantInt:     30 * time.Second,
			wantTimeout: 30 * time.Second,
			wantIO:      10 * time.Second,
		},
		{
			name:        "symmetric short flags for tests",
			args:        []string{"-addr", "127.0.0.1:0", "-heartbeat-interval", "2s", "-heartbeat-timeout", "2s"},
			wantInt:     2 * time.Second,
			wantTimeout: 2 * time.Second,
		},
		{
			name:    "zero interval refused",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-interval", "0s"},
			wantErr: true,
		},
		{
			name:    "negative interval refused",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-interval", "-5s"},
			wantErr: true,
		},
		{
			name:    "zero timeout refused",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-timeout", "0s"},
			wantErr: true,
		},
		{
			name:    "negative timeout refused",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-timeout", "-1s"},
			wantErr: true,
		},
		{
			name:    "interval beyond the day cap refused",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-interval", "48h"},
			wantErr: true,
		},
		{
			name:    "timeout beyond the day cap refused",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-timeout", "48h", "-heartbeat-interval", "47h"},
			wantErr: true,
		},
		{
			name:       "interval outgrowing timeout plus slack refused, naming both values",
			args:       []string{"-addr", "127.0.0.1:0", "-heartbeat-interval", "60s", "-heartbeat-timeout", "10s"},
			wantErr:    true,
			wantErrHas: []string{"1m0s", "10s"},
		},
		{
			name:    "interval exactly at timeout plus slack accepted",
			args:    []string{"-addr", "127.0.0.1:0", "-heartbeat-interval", "15s", "-heartbeat-timeout", "10s"},
			wantInt: 15 * time.Second, wantTimeout: 10 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// -no-auth is injected into every case: the auth gate (AC-6) runs
			// on the same surface, and these tests must isolate heartbeat
			// validation (AC-4) from it. Defaults stay untouched.
			cfg, err := loadConfig(append([]string{"-no-auth"}, tc.args...))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected startup refusal for %v, got cfg=%+v", tc.args, cfg)
				}
				for _, needle := range tc.wantErrHas {
					if !strings.Contains(err.Error(), needle) {
						t.Fatalf("error %q must name both offending values; missing %q", err, needle)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected startup error: %v", err)
			}
			if tc.wantInt != 0 && cfg.heartbeatInterval != tc.wantInt {
				t.Fatalf("heartbeatInterval = %v, want %v", cfg.heartbeatInterval, tc.wantInt)
			}
			if tc.wantTimeout != 0 && cfg.heartbeatTimeout != tc.wantTimeout {
				t.Fatalf("heartbeatTimeout = %v, want %v", cfg.heartbeatTimeout, tc.wantTimeout)
			}
			if tc.wantIO != 0 && cfg.ioTimeout != tc.wantIO {
				t.Fatalf("ioTimeout = %v, want %v", cfg.ioTimeout, tc.wantIO)
			}
		})
	}
}

// TestValidateHeartbeatConfigSlack pins the exact boundary of the D7
// invariant: interval may equal timeout + HeartbeatSlack but not exceed it.
func TestValidateHeartbeatConfigSlack(t *testing.T) {
	bound := 10*time.Second + transport.HeartbeatSlack
	if err := validateHeartbeatConfig(bound, 10*time.Second); err != nil {
		t.Fatalf("interval %v == timeout+slack must be accepted, got %v", bound, err)
	}
	if err := validateHeartbeatConfig(bound+time.Nanosecond, 10*time.Second); err == nil {
		t.Fatal("interval past timeout+slack must be refused")
	}
}
