package main

import (
	"os"
	"testing"
)

// TestParseFlags covers the shapes a user actually types. The flag parser is
// hand-written, so these cases are the contract: `-p 8080`, `--port=8080`,
// subcommands, and the two sentinel "not an error" results.
func TestParseFlags(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPort string
		wantRest []string
		wantErr  error
	}{
		{name: "no args", args: nil},
		{name: "short port", args: []string{"-p", "9000"}, wantPort: "9000"},
		{name: "long port", args: []string{"--port", "9001"}, wantPort: "9001"},
		{name: "equals form", args: []string{"--port=9002"}, wantPort: "9002"},
		{name: "single dash equals", args: []string{"-p=9003"}, wantPort: "9003"},
		{name: "subcommand", args: []string{"healthcheck"}, wantRest: []string{"healthcheck"}},
		{name: "flag before subcommand", args: []string{"-p", "9004", "healthcheck"}, wantPort: "9004", wantRest: []string{"healthcheck"}},
		{name: "help", args: []string{"-h"}, wantErr: errShowHelp},
		{name: "long help", args: []string{"--help"}, wantErr: errShowHelp},
		{name: "version", args: []string{"-v"}, wantErr: errShowVersion},
		{name: "double dash terminator", args: []string{"--", "-p", "9005"}, wantRest: []string{"-p", "9005"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, rest, err := parseFlags(tc.args)
			if tc.wantErr != nil {
				if err != tc.wantErr {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantPort != "" {
				if o.port == nil || *o.port != tc.wantPort {
					t.Fatalf("port = %v, want %q", o.port, tc.wantPort)
				}
			}
			if len(tc.wantRest) != len(rest) {
				t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
			}
			for i := range tc.wantRest {
				if rest[i] != tc.wantRest[i] {
					t.Fatalf("rest = %v, want %v", rest, tc.wantRest)
				}
			}
		})
	}
}

func TestParseFlagsMissingValue(t *testing.T) {
	if _, _, err := parseFlags([]string{"-p"}); err == nil {
		t.Fatal("-p without a value must fail")
	}
}

func TestParseFlagsUnknown(t *testing.T) {
	if _, _, err := parseFlags([]string{"--nope"}); err == nil {
		t.Fatal("unknown flag must fail")
	}
}

// TestOverridesApplyToEnv proves flags reach config.Load, which only reads the
// environment. Without this the flags would parse but be silently ignored.
func TestOverridesApplyToEnv(t *testing.T) {
	for _, k := range []string{"PORT", "HTTP_HOST", "STORE", "BROKER", "READONLY_SECRET", "APP_ENV"} {
		t.Setenv(k, "")
	}
	port := "9999"
	host := "127.0.0.1"
	secret := "x"
	prod := true
	overrides{port: &port, host: &host, secret: &secret, prodMode: &prod}.applyToEnv()

	if got := os.Getenv("PORT"); got != port {
		t.Errorf("PORT = %q", got)
	}
	if got := os.Getenv("HTTP_HOST"); got != host {
		t.Errorf("HTTP_HOST = %q", got)
	}
	if got := os.Getenv("APP_ENV"); got != "production" {
		t.Errorf("APP_ENV = %q", got)
	}
}

func TestPortOf(t *testing.T) {
	cases := map[string]string{":8080": "8080", "0.0.0.0:9000": "9000", "127.0.0.1:80": "80"}
	for in, want := range cases {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q) = %q, want %q", in, got, want)
		}
	}
}
