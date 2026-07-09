package main

import (
	"testing"
	"time"
)

// TestPickDelayInBounds: джиттер всегда попадает в [min, max].
func TestPickDelayInBounds(t *testing.T) {
	min, max := 200*time.Millisecond, 800*time.Millisecond
	for i := 0; i < 1000; i++ {
		d := pickDelay(min, max)
		if d < min || d > max {
			t.Fatalf("pickDelay вне [%s, %s]: %s", min, max, d)
		}
	}
}

// TestPickDelayEqual: при min == max (в т.ч. legacy --delay) — фиксированное значение.
func TestPickDelayEqual(t *testing.T) {
	if d := pickDelay(500*time.Millisecond, 500*time.Millisecond); d != 500*time.Millisecond {
		t.Fatalf("при min==max ожидалось 500ms, получено %s", d)
	}
}

func TestValidateDelayRange(t *testing.T) {
	tests := []struct {
		name     string
		min, max time.Duration
		wantErr  bool
	}{
		{"ok range", 200 * time.Millisecond, 800 * time.Millisecond, false},
		{"equal", time.Second, time.Second, false},
		{"min > max", 4 * time.Second, 2 * time.Second, true},
		{"negative min", -time.Second, time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDelayRange(tt.min, tt.max)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateDelayRange(%s, %s): err=%v, wantErr=%v", tt.min, tt.max, err, tt.wantErr)
			}
		})
	}
}

func TestValidateNoProxyWorkers(t *testing.T) {
	tests := []struct {
		name    string
		noProxy bool
		workers int
		wantErr bool
	}{
		{"no-proxy auto (0)", true, 0, false},
		{"no-proxy explicit 1", true, 1, false},
		{"no-proxy 3 → конфликт", true, 3, true},
		{"proxy mode 3 ok", false, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNoProxyWorkers(tt.noProxy, tt.workers)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateNoProxyWorkers(%v, %d): err=%v, wantErr=%v", tt.noProxy, tt.workers, err, tt.wantErr)
			}
		})
	}
}

func TestValidateLimitRate(t *testing.T) {
	tests := []struct {
		v       string
		wantErr bool
	}{
		{"", false}, {"0", false}, {"5M", false}, {"500K", false},
		{"1.5m", false}, {"1048576", false}, {"1G", false},
		{"abc", true}, {"5MB", true}, {"M5", true}, {"-1M", true},
	}
	for _, tt := range tests {
		if err := validateLimitRate(tt.v); (err != nil) != tt.wantErr {
			t.Errorf("validateLimitRate(%q): err=%v, wantErr=%v", tt.v, err, tt.wantErr)
		}
	}
}

// TestAcquireProxyNoProxy: в --no-proxy прокси не запрашивается (прямое соединение).
func TestAcquireProxyNoProxy(t *testing.T) {
	proxy, ok := acquireProxy(runConfig{noProxy: true}, nil, nil)
	if !ok || proxy != "" {
		t.Errorf("no-proxy: ожидалось (\"\", true), получено (%q, %v)", proxy, ok)
	}
}
