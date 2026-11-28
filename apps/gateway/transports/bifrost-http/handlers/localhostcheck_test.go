package handlers

import "testing"

func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"127.0.0.1", true},
		{"127.0.0.1:8080", true},
		{"::1", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"::ffff:127.0.0.1", true},
		{"", false},
		{"[::1", false},
		{"::1]", false},
		{"[localhost]", false},
		{"example.com", false},
		{"example.com:8080", false},
		{"evil-localhost.com:80", false},
		{"192.168.1.10:8080", false},
		{"[2001:db8::1]:8080", false},
		{"2001:db8::1", false},
	}
	for _, tt := range tests {
		if got := isLocalhost(tt.host); got != tt.want {
			t.Errorf("isLocalhost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestIsLocalhostOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"http://localhost:3000", true},
		{"https://localhost:3000", true},
		{"http://127.0.0.1:3000", true},
		{"https://127.0.0.1:3000", true},
		{"http://0.0.0.0:3000", true},
		{"http://[::1]:3000", true},
		{"https://[::1]:3000", true},
		{"http://[::]:3000", true},
		{"http://localhost", true},
		{"http://example.com:3000", false},
		{"https://evil.com", false},
		{"http://[2001:db8::1]:3000", false},
		{"ftp://localhost:3000", false},
		{"not-a-url", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLocalhostOrigin(tt.origin); got != tt.want {
			t.Errorf("isLocalhostOrigin(%q) = %v, want %v", tt.origin, got, tt.want)
		}
	}
}
