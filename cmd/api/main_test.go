package main

import "testing"

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"postgres://shortener:shortener@localhost:5432/shortener",
			"postgres://shortener:***@localhost:5432/shortener",
		},
		{
			"postgres://user:p%40ss@host/db?sslmode=require",
			"postgres://user:***@host/db?sslmode=require",
		},
		{
			// No password component — pass through unchanged.
			"postgres://user@host/db",
			"postgres://user@host/db",
		},
		{
			// No userinfo at all — pass through.
			"postgres://host/db",
			"postgres://host/db",
		},
		{
			// Unparseable shape — pass through rather than crash.
			"not-a-dsn",
			"not-a-dsn",
		},
	}
	for _, c := range cases {
		got := redactDSN(c.in)
		if got != c.want {
			t.Errorf("redactDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
