package outlookimap

import (
	"errors"
	"testing"
	"time"

	"github.com/emersion/go-imap"
)

func TestXOAUTH2Payload(t *testing.T) {
	auth := XOAUTH2("user@hotmail.com", "access-token")
	mech, payload, err := auth.Start()
	if err != nil {
		t.Fatalf("Start() returned error: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Fatalf("mechanism = %q, want XOAUTH2", mech)
	}

	want := "user=user@hotmail.com\x01auth=Bearer access-token\x01\x01"
	if string(payload) != want {
		t.Fatalf("payload = %q, want %q", payload, want)
	}
}

func TestSearchCriteriaUsesUnreadHeaderFilters(t *testing.T) {
	criteria := searchCriteria("Verify", "account-security-noreply@accountprotection.microsoft.com")

	if got := criteria.Header.Get("Subject"); got != "Verify" {
		t.Fatalf("Subject header = %q", got)
	}
	if got := criteria.Header.Get("From"); got != "account-security-noreply@accountprotection.microsoft.com" {
		t.Fatalf("From header = %q", got)
	}
	if len(criteria.WithoutFlags) != 1 || criteria.WithoutFlags[0] != imap.SeenFlag {
		t.Fatalf("WithoutFlags = %#v, want only Seen flag", criteria.WithoutFlags)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := ImapConfig{}

	if cfg.imapServer() != defaultIMAPServer {
		t.Fatalf("imapServer() = %q", cfg.imapServer())
	}
	if cfg.pollTimeout() != defaultPollTimeout {
		t.Fatalf("pollTimeout() = %v", cfg.pollTimeout())
	}
	if cfg.reconnectDelay() != defaultReconnectDelay {
		t.Fatalf("reconnectDelay() = %v", cfg.reconnectDelay())
	}

	cfg.PollTimeout = time.Second
	if cfg.pollTimeout() != time.Second {
		t.Fatalf("pollTimeout() override = %v", cfg.pollTimeout())
	}

	cfg.ReconnectDelay = 500 * time.Millisecond
	if cfg.reconnectDelay() != 500*time.Millisecond {
		t.Fatalf("reconnectDelay() override = %v", cfg.reconnectDelay())
	}
}

func TestResolveAuthMethod(t *testing.T) {
	tests := []struct {
		name string
		cfg  ImapConfig
		want AuthMethod
	}{
		{
			name: "auto prefers xoauth2 token",
			cfg:  ImapConfig{Token: "access-token", Password: "password"},
			want: AuthXOAUTH2,
		},
		{
			name: "auto uses password when token is empty",
			cfg:  ImapConfig{Password: "password"},
			want: AuthPassword,
		},
		{
			name: "explicit xoauth2",
			cfg:  ImapConfig{AuthMethod: AuthXOAUTH2, Token: "access-token"},
			want: AuthXOAUTH2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.resolveAuthMethod()
			if err != nil {
				t.Fatalf("resolveAuthMethod() returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveAuthMethod() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateAuthMethod(t *testing.T) {
	if err := (&ImapConfig{}).validateAuth(AuthXOAUTH2); err == nil {
		t.Fatal("validateAuth(AuthXOAUTH2) succeeded without token")
	}
	if err := (&ImapConfig{Token: "access-token"}).validateAuth(AuthXOAUTH2); err != nil {
		t.Fatalf("validateAuth(AuthXOAUTH2) returned error: %v", err)
	}
	if err := (&ImapConfig{}).validateAuth(AuthPassword); err == nil {
		t.Fatal("validateAuth(AuthPassword) succeeded without password")
	}
}

func TestIsTransientIMAPError(t *testing.T) {
	transient := []string{
		"EOF",
		"imap: connection closed",
		"read tcp 127.0.0.1:1: i/o timeout",
		"write tcp 127.0.0.1:1: broken pipe",
	}

	for _, msg := range transient {
		if !isTransientIMAPError(errors.New(msg)) {
			t.Fatalf("isTransientIMAPError(%q) = false, want true", msg)
		}
	}

	if isTransientIMAPError(errors.New("NO AUTHENTICATE failed")) {
		t.Fatal("auth failure should not be treated as transient")
	}
}
