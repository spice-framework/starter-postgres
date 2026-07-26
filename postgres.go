// Package postgres provides a reviewed pgx-backed database/sql starter for
// Spice applications.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultApplicationName       = "spice"
	defaultMaxOpenConnections    = 20
	defaultMaxIdleConnections    = 10
	defaultConnectionMaxLifetime = 30 * time.Minute
	defaultConnectionMaxIdleTime = 5 * time.Minute
	maxConnectionURLBytes        = 16 << 10
	maxApplicationNameBytes      = 63
	maxConfiguredConnectionCount = 100_000
)

// Options defines a PostgreSQL pool. URL must be a complete postgres or
// postgresql URL so connection behavior never falls back to process
// environment variables.
type Options struct {
	URL                   string
	ApplicationName       string
	MaxOpenConnections    int
	MaxIdleConnections    int
	ConnectionMaxLifetime time.Duration
	ConnectionMaxIdleTime time.Duration
	AllowInsecure         bool
}

// Open validates a complete connection URL and constructs a caller-owned
// database/sql pool. It performs no network I/O.
func Open(options Options) (*sql.DB, error) {
	normalized, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfigWithOptions(normalized.URL, pgx.ParseConfigOptions{
		ParseConfigOptions: pgconn.ParseConfigOptions{
			ConnStringAllowedKeys: allowedConnectionURLKeys(),
		},
	})
	if err != nil || config == nil {
		return nil, errors.New("construct PostgreSQL database: connection URL is invalid")
	}
	if config.RuntimeParams == nil {
		config.RuntimeParams = make(map[string]string)
	}
	config.RuntimeParams["application_name"] = normalized.ApplicationName

	database := stdlib.OpenDB(*config)
	database.SetMaxOpenConns(normalized.MaxOpenConnections)
	database.SetMaxIdleConns(normalized.MaxIdleConnections)
	database.SetConnMaxLifetime(normalized.ConnectionMaxLifetime)
	database.SetConnMaxIdleTime(normalized.ConnectionMaxIdleTime)
	return database, nil
}

// Ping verifies one caller-owned database with the supplied context.
func Ping(ctx context.Context, database *sql.DB) error {
	switch {
	case ctx == nil:
		return errors.New("ping PostgreSQL database: context is nil")
	case database == nil:
		return errors.New("ping PostgreSQL database: database is nil")
	}
	if err := database.PingContext(ctx); err != nil {
		return fmt.Errorf("ping PostgreSQL database: %w", err)
	}
	return nil
}

func normalizeOptions(options Options) (Options, error) {
	normalizedURL, err := normalizeURL(options.URL, options.AllowInsecure)
	if err != nil {
		return Options{}, err
	}
	options.URL = normalizedURL
	if options.ApplicationName == "" {
		options.ApplicationName = defaultApplicationName
	}
	if !validApplicationName(options.ApplicationName) {
		return Options{}, errors.New("construct PostgreSQL database: application name is invalid")
	}

	if err := normalizePool(&options); err != nil {
		return Options{}, err
	}
	return options, nil
}

func normalizeURL(rawURL string, allowInsecure bool) (string, error) {
	if rawURL == "" || len(rawURL) > maxConnectionURLBytes {
		return "", errors.New("construct PostgreSQL database: connection URL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil {
		return "", errors.New("construct PostgreSQL database: connection URL is invalid")
	}
	if !validNetworkIdentity(parsed) || !validDatabaseIdentity(parsed) {
		return "", errors.New("construct PostgreSQL database: connection URL is incomplete")
	}

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", errors.New("construct PostgreSQL database: connection URL is invalid")
	}
	sslModes, present := query["sslmode"]
	if !present {
		query.Set("sslmode", "verify-full")
	} else if len(sslModes) != 1 || !validSSLMode(sslModes[0], allowInsecure) {
		return "", errors.New("construct PostgreSQL database: sslmode is not permitted")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func validNetworkIdentity(parsed *url.URL) bool {
	return (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") &&
		parsed.Host != "" &&
		parsed.Hostname() != "" &&
		!strings.Contains(parsed.Host, ",") &&
		validPort(parsed.Port())
}

func validDatabaseIdentity(parsed *url.URL) bool {
	return parsed.User != nil &&
		parsed.User.Username() != "" &&
		hasPassword(parsed.User) &&
		parsed.Path != "" &&
		parsed.Path != "/" &&
		parsed.Fragment == ""
}

func validPort(port string) bool {
	value, err := strconv.ParseUint(port, 10, 16)
	return err == nil && value > 0
}

func hasPassword(user *url.Userinfo) bool {
	password, present := user.Password()
	return present && password != ""
}

func allowedConnectionURLKeys() []string {
	return []string{
		"channel_binding",
		"connect_timeout",
		"database",
		"default_query_exec_mode",
		"host",
		"max_protocol_version",
		"min_protocol_version",
		"password",
		"port",
		"require_auth",
		"sslcert",
		"sslkey",
		"sslmode",
		"sslnegotiation",
		"sslpassword",
		"sslrootcert",
		"sslsni",
		"target_session_attrs",
		"user",
	}
}

func validSSLMode(mode string, allowInsecure bool) bool {
	switch mode {
	case "verify-full", "verify-ca", "require":
		return true
	case "disable":
		return allowInsecure
	default:
		return false
	}
}

func validApplicationName(name string) bool {
	if name == "" || len(name) > maxApplicationNameBytes {
		return false
	}
	for _, character := range []byte(name) {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

func normalizePool(options *Options) error {
	if options.MaxOpenConnections == 0 {
		options.MaxOpenConnections = defaultMaxOpenConnections
	}
	if options.MaxIdleConnections == 0 {
		options.MaxIdleConnections = defaultMaxIdleConnections
	}
	if options.ConnectionMaxLifetime == 0 {
		options.ConnectionMaxLifetime = defaultConnectionMaxLifetime
	}
	if options.ConnectionMaxIdleTime == 0 {
		options.ConnectionMaxIdleTime = defaultConnectionMaxIdleTime
	}

	switch {
	case options.MaxOpenConnections < 1 ||
		options.MaxOpenConnections > maxConfiguredConnectionCount:
		return errors.New("construct PostgreSQL database: max open connections is invalid")
	case options.MaxIdleConnections < 1 ||
		options.MaxIdleConnections > options.MaxOpenConnections:
		return errors.New("construct PostgreSQL database: max idle connections is invalid")
	case options.ConnectionMaxLifetime < 0:
		return errors.New("construct PostgreSQL database: connection max lifetime is invalid")
	case options.ConnectionMaxIdleTime < 0:
		return errors.New("construct PostgreSQL database: connection max idle time is invalid")
	default:
		return nil
	}
}
