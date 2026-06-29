package main

import "testing"

func TestParseProxy(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		scheme  string
		want    string
		wantErr bool
	}{
		// scheme "http" — все 4 формата
		{
			name:   "http: ready URL pass-through",
			raw:    "http://alice:secret@1.2.3.4:8080",
			scheme: "http",
			want:   "http://alice:secret@1.2.3.4:8080",
		},
		{
			name:   "http: USER:PASS@IP:PORT",
			raw:    "alice:secret@1.2.3.4:8080",
			scheme: "http",
			want:   "http://alice:secret@1.2.3.4:8080",
		},
		{
			name:   "http: IP:PORT:USER:PASS",
			raw:    "1.2.3.4:8080:alice:secret",
			scheme: "http",
			want:   "http://alice:secret@1.2.3.4:8080",
		},
		{
			name:   "http: IP:PORT",
			raw:    "1.2.3.4:8080",
			scheme: "http",
			want:   "http://1.2.3.4:8080",
		},

		// scheme "socks5" — все 4 формата
		{
			name:   "socks5: ready URL pass-through",
			raw:    "socks5://alice:secret@1.2.3.4:1080",
			scheme: "socks5",
			want:   "socks5://alice:secret@1.2.3.4:1080",
		},
		{
			name:   "socks5: USER:PASS@IP:PORT",
			raw:    "alice:secret@1.2.3.4:1080",
			scheme: "socks5",
			want:   "socks5://alice:secret@1.2.3.4:1080",
		},
		{
			name:   "socks5: IP:PORT:USER:PASS",
			raw:    "1.2.3.4:1080:alice:secret",
			scheme: "socks5",
			want:   "socks5://alice:secret@1.2.3.4:1080",
		},
		{
			name:   "socks5: IP:PORT",
			raw:    "1.2.3.4:1080",
			scheme: "socks5",
			want:   "socks5://1.2.3.4:1080",
		},

		// IPv6 со схемой — pass-through (требуется полная схема с [])
		{
			name:   "http: IPv6 with scheme pass-through",
			raw:    "http://[2001:db8::1]:8080",
			scheme: "http",
			want:   "http://[2001:db8::1]:8080",
		},
		{
			name:   "socks5: IPv6 with scheme pass-through",
			raw:    "socks5://[2001:db8::1]:1080",
			scheme: "socks5",
			want:   "socks5://[2001:db8::1]:1080",
		},

		// USER:PASS@IP:PORT с паролем, содержащим ':' — ветка с @ не дробит
		{
			name:   "http: USER:PASS@IP:PORT password with colon",
			raw:    "alice:sec:ret@1.2.3.4:8080",
			scheme: "http",
			want:   "http://alice:sec:ret@1.2.3.4:8080",
		},
		{
			name:   "socks5: USER:PASS@IP:PORT password with multiple colons",
			raw:    "bob:p:a:s:s@1.2.3.4:1080",
			scheme: "socks5",
			want:   "socks5://bob:p:a:s:s@1.2.3.4:1080",
		},

		// невалидный формат
		{
			name:    "invalid: three parts",
			raw:     "1.2.3.4:8080:onlythree",
			scheme:  "http",
			wantErr: true,
		},
		{
			name:    "invalid: five parts",
			raw:     "1.2.3.4:8080:user:pass:extra",
			scheme:  "http",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxy(tt.raw, tt.scheme)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseProxy(%q, %q) expected error, got %q", tt.raw, tt.scheme, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseProxy(%q, %q) unexpected error: %v", tt.raw, tt.scheme, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseProxy(%q, %q) = %q, want %q", tt.raw, tt.scheme, got, tt.want)
			}
		})
	}
}
