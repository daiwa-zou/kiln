package config

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies([]string{
		"10.0.0.0/8",
		" 192.0.2.10 ", // bare IPv4: one host
		"2001:db8::/32",
		"2001:db8::1",           // bare IPv6
		"fe80::1%eth0",          // a zone means nothing to a prefix match
		"::ffff:172.16.0.0/108", // mapped: matched against unmapped peers
		"10.1.2.3/8",            // host bits are masked off
		"",                      // trailing comma in the env var
	})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.10/32", "2001:db8::/32", "2001:db8::1/128", "fe80::1/128", "172.16.0.0/12", "10.0.0.0/8"}
	var gotS []string
	for _, p := range got {
		gotS = append(gotS, p.String())
	}
	if !slices.Equal(gotS, want) {
		t.Errorf("prefixes = %v, want %v", gotS, want)
	}

	if got, err := ParseTrustedProxies(nil); err != nil || len(got) != 0 {
		t.Errorf("empty list = %v, %v; want nothing, no error", got, err)
	}

	_, err = ParseTrustedProxies([]string{"10.0.0.0/8", "proxy.internal", "10.0.0.0/33", "300.1.1.1"})
	if err == nil {
		t.Fatal("bad entries were accepted")
	}
	for _, bad := range []string{"proxy.internal", "10.0.0.0/33", "300.1.1.1"} {
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("error does not name %q: %v", bad, err)
		}
	}
}

// A bad entry fails startup through Validate, with the other problems.
func TestValidateRejectsBadTrustedProxy(t *testing.T) {
	cfg := &Config{
		Role: RoleServer, HTTPAddr: ":8080",
		Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
		Storage:  Storage{Backend: BackendFS, Path: "/tmp/blobs"},
		Agent:    Agent{Runner: RunnerFake, Timeout: time.Minute, MaxPagesPerRun: 12, UnitConcurrency: 1},
	}
	cfg.TrustedProxies = []string{"10.0.0.0/8", "not-a-cidr"}
	err := cfg.Validate()
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("Validate = %v, want an ErrInvalid naming trusted_proxies", err)
	}
	cfg.TrustedProxies = []string{"10.0.0.0/8"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate on a good list: %v", err)
	}
}

func TestTrustedProxyWarnings(t *testing.T) {
	cases := []struct {
		prefix string
		want   string // substring; "" means no warning
	}{
		{"0.0.0.0/0", "every address"},
		{"::/0", "every address"},
		{"10.0.0.0/8", "broad"},
		{"10.0.0.0/7", "broad"},
		{"10.0.0.0/9", ""},
		{"2001:db8::/32", "broad"},
		{"2001::/16", "broad"},
		{"2001:db8::/33", ""},
		{"192.0.2.10/32", ""},
	}
	for _, tc := range cases {
		got := TrustedProxyWarnings([]netip.Prefix{netip.MustParsePrefix(tc.prefix)})
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%s: unexpected warning %q", tc.prefix, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want) || !strings.Contains(got[0], tc.prefix)):
			t.Errorf("%s: warnings = %q, want one naming the prefix and containing %q", tc.prefix, got, tc.want)
		}
	}
	if got := TrustedProxyWarnings(nil); len(got) != 0 {
		t.Errorf("no prefixes produced warnings %q", got)
	}
}

func TestTrustedProxiesEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("KILN_TRUSTED_PROXIES", "10.0.0.0/8, 192.0.2.10 ,2001:db8::/32")
	cfg, err := Load(Options{Role: RoleServer})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", cfg.TrustedProxies, err)
	}
	if len(got) != 3 || got[1].String() != "192.0.2.10/32" {
		t.Errorf("trusted proxies from env = %v (raw %q)", got, cfg.TrustedProxies)
	}

	t.Setenv("KILN_TRUSTED_PROXIES", "10.0.0.0/8,garbage")
	if _, err := Load(Options{Role: RoleServer}); err == nil || !strings.Contains(err.Error(), "garbage") {
		t.Errorf("Load with a bad entry = %v, want an error naming it", err)
	}
}

func TestTrustedProxiesDefaultEmpty(t *testing.T) {
	isolateEnv(t)
	cfg, err := Load(Options{Role: RoleServer})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("default trusted_proxies = %q, want empty", cfg.TrustedProxies)
	}
}
