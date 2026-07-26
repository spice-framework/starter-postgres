package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOpenConfiguresPoolWithoutConnecting(t *testing.T) {
	t.Parallel()

	database, err := Open(Options{
		URL: "postgres://spice:secret@127.0.0.1:1/spice",
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("close database: %v", closeErr)
		}
	})
	if database.Stats().MaxOpenConnections != defaultMaxOpenConnections {
		t.Fatalf("unexpected max open connections: %d", database.Stats().MaxOpenConnections)
	}
}

func TestNormalizeOptionsAppliesSecureDeterministicDefaults(t *testing.T) {
	t.Parallel()

	options, err := normalizeOptions(Options{
		URL: "postgres://spice:secret@database.example.test:5432/application",
	})
	if err != nil {
		t.Fatalf("normalize options: %v", err)
	}
	if !strings.Contains(options.URL, "sslmode=verify-full") {
		t.Fatalf("secure sslmode was not applied: %s", options.URL)
	}
	if options.ApplicationName != defaultApplicationName ||
		options.MaxOpenConnections != defaultMaxOpenConnections ||
		options.MaxIdleConnections != defaultMaxIdleConnections ||
		options.ConnectionMaxLifetime != defaultConnectionMaxLifetime ||
		options.ConnectionMaxIdleTime != defaultConnectionMaxIdleTime {
		t.Fatalf("unexpected defaults: %#v", options)
	}
}

func TestNormalizeOptionsPreservesExplicitPoolAndTLS(t *testing.T) {
	t.Parallel()

	options, err := normalizeOptions(Options{
		URL:                   "postgresql://spice:secret@database.example.test:5432/application?sslmode=require",
		ApplicationName:       "orders-service",
		MaxOpenConnections:    50,
		MaxIdleConnections:    25,
		ConnectionMaxLifetime: time.Hour,
		ConnectionMaxIdleTime: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("normalize options: %v", err)
	}
	if options.ApplicationName != "orders-service" ||
		options.MaxOpenConnections != 50 ||
		options.MaxIdleConnections != 25 ||
		options.ConnectionMaxLifetime != time.Hour ||
		options.ConnectionMaxIdleTime != 10*time.Minute {
		t.Fatalf("explicit options changed: %#v", options)
	}
}

func TestNormalizeOptionsValidatesBoundariesWithoutExposingSecrets(t *testing.T) {
	t.Parallel()

	valid := Options{
		URL: "postgres://spice:super-secret@database.example.test:5432/application",
	}
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "empty URL", mutate: func(options *Options) { options.URL = "" }},
		{name: "large URL", mutate: func(options *Options) {
			options.URL = "postgres://" + strings.Repeat("x", maxConnectionURLBytes)
		}},
		{name: "scheme", mutate: func(options *Options) {
			options.URL = "mysql://spice:secret@database.example.test:5432/application"
		}},
		{name: "host", mutate: func(options *Options) { options.URL = "postgres://spice:secret@/application" }},
		{name: "user", mutate: func(options *Options) {
			options.URL = "postgres://:secret@database.example.test:5432/application"
		}},
		{name: "password", mutate: func(options *Options) {
			options.URL = "postgres://spice@database.example.test:5432/application"
		}},
		{name: "port", mutate: func(options *Options) {
			options.URL = "postgres://spice:secret@database.example.test/application"
		}},
		{name: "database", mutate: func(options *Options) {
			options.URL = "postgres://spice:secret@database.example.test:5432/"
		}},
		{name: "fragment", mutate: func(options *Options) { options.URL += "#fragment" }},
		{name: "duplicate sslmode", mutate: func(options *Options) {
			options.URL += "?sslmode=require&sslmode=verify-full"
		}},
		{name: "malformed query", mutate: func(options *Options) {
			options.URL += "?sslmode=%zz"
		}},
		{name: "file-backed service", mutate: func(options *Options) {
			options.URL += "?servicefile=redirect.conf"
		}},
		{name: "preferred sslmode", mutate: func(options *Options) {
			options.URL += "?sslmode=prefer"
		}},
		{name: "insecure sslmode", mutate: func(options *Options) {
			options.URL += "?sslmode=disable"
		}},
		{name: "application control", mutate: func(options *Options) {
			options.ApplicationName = "orders\nservice"
		}},
		{name: "application length", mutate: func(options *Options) {
			options.ApplicationName = strings.Repeat("x", maxApplicationNameBytes+1)
		}},
		{name: "max open negative", mutate: func(options *Options) {
			options.MaxOpenConnections = -1
		}},
		{name: "max open excess", mutate: func(options *Options) {
			options.MaxOpenConnections = maxConfiguredConnectionCount + 1
		}},
		{name: "max idle negative", mutate: func(options *Options) {
			options.MaxIdleConnections = -1
		}},
		{name: "max idle excess", mutate: func(options *Options) {
			options.MaxOpenConnections = 2
			options.MaxIdleConnections = 3
		}},
		{name: "lifetime", mutate: func(options *Options) {
			options.ConnectionMaxLifetime = -time.Second
		}},
		{name: "idle time", mutate: func(options *Options) {
			options.ConnectionMaxIdleTime = -time.Second
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			options := valid
			test.mutate(&options)
			_, err := Open(options)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Fatalf("error exposed connection secret: %v", err)
			}
		})
	}
}

func TestNormalizeOptionsAllowsExplicitLocalInsecurity(t *testing.T) {
	t.Parallel()

	options, err := normalizeOptions(Options{
		URL:           "postgres://spice:secret@127.0.0.1:5432/spice?sslmode=disable",
		AllowInsecure: true,
	})
	if err != nil {
		t.Fatalf("normalize options: %v", err)
	}
	if !strings.Contains(options.URL, "sslmode=disable") {
		t.Fatalf("insecure opt-in was not retained: %s", options.URL)
	}
}

func TestPingValidatesInputsAndPreservesCancellation(t *testing.T) {
	t.Parallel()

	if err := Ping(nilTestContext(), nil); err == nil ||
		!strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("expected nil context error, got %v", err)
	}
	if err := Ping(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "database is nil") {
		t.Fatalf("expected nil database error, got %v", err)
	}

	database, err := Open(Options{
		URL:           "postgres://spice:secret@127.0.0.1:1/spice?sslmode=disable",
		AllowInsecure: true,
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := database.Close(); closeErr != nil {
			t.Errorf("close database: %v", closeErr)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Ping(ctx, database); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func nilTestContext() context.Context {
	return nil
}
