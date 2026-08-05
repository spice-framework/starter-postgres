// Package spicetest provides deterministic test slices for generated Spice
// applications.
package spicetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spice-framework/spice/lifecycle"
	"github.com/spice-framework/spice/web"
)

const (
	defaultTestTimeout      = 5 * time.Second
	maxTestTimeout          = time.Minute
	defaultMaxRequestBytes  = int64(1 << 20)
	defaultMaxResponseBytes = int64(1 << 20)
	maxTestBodyBytes        = int64(64 << 20)
)

// HTTPApplication is the generated surface required by an HTTP test slice.
// The slice does not start application lifecycle hooks.
type HTTPApplication interface {
	Handler() http.Handler
	Stop(context.Context) error
	State() lifecycle.State
}

// HTTPFactory constructs one generated application with caller-selected test
// configuration and overrides.
type HTTPFactory func(context.Context) (HTTPApplication, error)

// ListenFunc creates the slice's loopback listener. It is primarily a
// deterministic failure seam; nil selects net.ListenConfig.Listen.
type ListenFunc func(context.Context, string, string) (net.Listener, error)

// HTTPOptions bounds the local server, client, cleanup, and response body.
type HTTPOptions struct {
	ClientTimeout        time.Duration
	ShutdownTimeout      time.Duration
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
	Listen               ListenFunc
}

// HTTPRequest describes one bounded request to a test slice. Path must be a
// root-relative request target. JSON and Body are mutually exclusive.
type HTTPRequest struct {
	Method string
	Path   string
	Header http.Header
	JSON   any
	Body   []byte
}

// HTTPResponse is a detached bounded response.
type HTTPResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// DecodeJSON strictly decodes one JSON value from the detached body.
func (response HTTPResponse) DecodeJSON(target any) error {
	if target == nil {
		return errors.New("decode test response JSON: target is nil")
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode test response JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode test response JSON: multiple values")
		}
		return fmt.Errorf("decode test response JSON: trailing content: %w", err)
	}
	return nil
}

// Problem decodes and validates one RFC 9457 response.
func (response HTTPResponse) Problem() (web.Problem, error) {
	var problem web.Problem
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/problem+json" {
		return problem, errors.New(
			"decode test problem: content type is not application/problem+json",
		)
	}
	if err := response.DecodeJSON(&problem); err != nil {
		return problem, fmt.Errorf("decode test problem: %w", err)
	}
	if err := problem.Validate(); err != nil {
		return problem, fmt.Errorf("decode test problem: %w", err)
	}
	if problem.Status != response.StatusCode {
		return problem, fmt.Errorf(
			"decode test problem: document status %d differs from HTTP status %d",
			problem.Status,
			response.StatusCode,
		)
	}
	return problem, nil
}

// HTTP is an isolated loopback HTTP slice around one generated application.
type HTTP struct {
	application     HTTPApplication
	server          *http.Server
	client          *http.Client
	baseURL         string
	maxRequestBody  int64
	maxResponseBody int64
	shutdownTimeout time.Duration
	serveDone       chan error
	closed          atomic.Bool
	closeOnce       sync.Once
	closeErr        error
}

// NewHTTP constructs an application, validates its generated HTTP surface,
// and starts one loopback-only server. It never starts lifecycle hooks, owns
// process signals, or reads process configuration itself.
func NewHTTP(
	ctx context.Context,
	factory HTTPFactory,
	options HTTPOptions,
) (*HTTP, error) {
	normalized, err := normalizeHTTPOptions(options)
	if err != nil {
		return nil, err
	}
	switch {
	case ctx == nil:
		return nil, errors.New("construct HTTP test slice: context is nil")
	case factory == nil:
		return nil, errors.New("construct HTTP test slice: factory is nil")
	default:
		if cause := context.Cause(ctx); cause != nil {
			return nil, fmt.Errorf("construct HTTP test slice: %w", cause)
		}
	}

	application, err := factory(ctx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("construct HTTP test application: %w", err),
			stopFailedApplication(application, normalized.ShutdownTimeout),
		)
	}
	if validationErr := validateHTTPApplication(application); validationErr != nil {
		return nil, errors.Join(
			validationErr,
			stopFailedApplication(application, normalized.ShutdownTimeout),
		)
	}

	listener, err := normalized.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("listen for HTTP test slice: %w", err),
			stopFailedApplication(application, normalized.ShutdownTimeout),
		)
	}
	if listener == nil {
		return nil, errors.Join(
			errors.New("listen for HTTP test slice: listener is nil"),
			stopFailedApplication(application, normalized.ShutdownTimeout),
		)
	}

	server := &http.Server{
		Handler:           application.Handler(),
		ReadHeaderTimeout: defaultTestTimeout,
		ReadTimeout:       defaultTestTimeout,
		WriteTimeout:      defaultTestTimeout,
		IdleTimeout:       defaultTestTimeout,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: defaultTestTimeout}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       defaultTestTimeout,
		TLSHandshakeTimeout:   defaultTestTimeout,
		ExpectContinueTimeout: time.Second,
	}
	slice := &HTTP{
		application:     application,
		server:          server,
		client:          &http.Client{Transport: transport, Timeout: normalized.ClientTimeout},
		baseURL:         "http://" + listener.Addr().String(),
		maxRequestBody:  normalized.MaxRequestBodyBytes,
		maxResponseBody: normalized.MaxResponseBodyBytes,
		shutdownTimeout: normalized.ShutdownTimeout,
		serveDone:       make(chan error, 1),
	}
	go func() {
		serveErr := server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		slice.serveDone <- serveErr
	}()
	return slice, nil
}

// URL returns the loopback base URL.
func (slice *HTTP) URL() string {
	if slice == nil {
		return ""
	}
	return slice.baseURL
}

// Application returns the generated application used by this slice.
func (slice *HTTP) Application() HTTPApplication {
	if slice == nil {
		return nil
	}
	return slice.application
}

// Do sends one bounded request and fully detaches its response body.
func (slice *HTTP) Do(
	ctx context.Context,
	spec HTTPRequest,
) (HTTPResponse, error) {
	if err := validateHTTPRequest(ctx, slice, spec); err != nil {
		return HTTPResponse{}, err
	}
	body, err := requestBody(spec, slice.maxRequestBody)
	if err != nil {
		return HTTPResponse{}, err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		spec.Method,
		slice.baseURL+spec.Path,
		bytes.NewReader(body),
	)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("construct HTTP test request: %w", err)
	}
	request.Header = spec.Header.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	if spec.JSON != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if request.Header.Get("Accept") == "" {
		request.Header.Set("Accept", "application/json")
	}
	response, err := slice.client.Do(request)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("execute HTTP test request: %w", err)
	}
	limited := io.LimitReader(response.Body, slice.maxResponseBody+1)
	content, readErr := io.ReadAll(limited)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return HTTPResponse{}, fmt.Errorf("read HTTP test response: %w", err)
	}
	if int64(len(content)) > slice.maxResponseBody {
		return HTTPResponse{}, fmt.Errorf(
			"read HTTP test response: body exceeds %d bytes",
			slice.maxResponseBody,
		)
	}
	return HTTPResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
		Body:       content,
	}, nil
}

// Close uses a fresh bounded background context to stop the local server and
// generated application. It is idempotent and concurrency-safe.
func (slice *HTTP) Close() error {
	if slice == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), slice.shutdownTimeout)
	defer cancel()
	return slice.CloseContext(ctx)
}

// CloseContext stops the local server and generated application with a
// caller-owned context. A shutdown failure forces the local listener closed.
func (slice *HTTP) CloseContext(ctx context.Context) error {
	if slice == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("close HTTP test slice: context is nil")
	}
	slice.closeOnce.Do(func() {
		slice.closed.Store(true)
		shutdownErr := slice.server.Shutdown(ctx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, slice.server.Close())
		}
		slice.client.CloseIdleConnections()
		serveErr := <-slice.serveDone
		stopErr := slice.application.Stop(ctx)
		slice.closeErr = errors.Join(
			wrapTestCloseError("shutdown local HTTP server", shutdownErr),
			wrapTestCloseError("serve local HTTP server", serveErr),
			wrapTestCloseError("stop generated application", stopErr),
		)
	})
	return slice.closeErr
}

func normalizeHTTPOptions(options HTTPOptions) (HTTPOptions, error) {
	if options.ClientTimeout == 0 {
		options.ClientTimeout = defaultTestTimeout
	}
	if options.ShutdownTimeout == 0 {
		options.ShutdownTimeout = defaultTestTimeout
	}
	if options.MaxResponseBodyBytes == 0 {
		options.MaxResponseBodyBytes = defaultMaxResponseBytes
	}
	if options.MaxRequestBodyBytes == 0 {
		options.MaxRequestBodyBytes = defaultMaxRequestBytes
	}
	if options.ClientTimeout < 0 || options.ClientTimeout > maxTestTimeout {
		return HTTPOptions{}, errors.New(
			"construct HTTP test slice: client timeout must be between 1ns and 1m",
		)
	}
	if options.ShutdownTimeout < 0 || options.ShutdownTimeout > maxTestTimeout {
		return HTTPOptions{}, errors.New(
			"construct HTTP test slice: shutdown timeout must be between 1ns and 1m",
		)
	}
	if options.MaxRequestBodyBytes < 0 ||
		options.MaxRequestBodyBytes > maxTestBodyBytes {
		return HTTPOptions{}, errors.New(
			"construct HTTP test slice: request body limit must be between 1 and 67108864 bytes",
		)
	}
	if options.MaxResponseBodyBytes < 0 ||
		options.MaxResponseBodyBytes > maxTestBodyBytes {
		return HTTPOptions{}, errors.New(
			"construct HTTP test slice: response body limit must be between 1 and 67108864 bytes",
		)
	}
	if options.Listen == nil {
		listenConfig := &net.ListenConfig{}
		options.Listen = listenConfig.Listen
	}
	return options, nil
}

func validateHTTPApplication(application HTTPApplication) error {
	if application == nil {
		return errors.New("construct HTTP test slice: application is nil")
	}
	state := application.State()
	if state != lifecycle.StateConstructed && state != lifecycle.StateReady {
		return fmt.Errorf(
			"construct HTTP test slice: application state %s is not usable",
			state,
		)
	}
	if application.Handler() == nil {
		return errors.New("construct HTTP test slice: application handler is nil")
	}
	return nil
}

func validateHTTPRequest(
	ctx context.Context,
	slice *HTTP,
	spec HTTPRequest,
) error {
	switch {
	case ctx == nil:
		return errors.New("execute HTTP test request: context is nil")
	case slice == nil || slice.server == nil || slice.client == nil:
		return errors.New("execute HTTP test request: slice is nil")
	case slice.closed.Load():
		return errors.New("execute HTTP test request: slice is closed")
	case strings.TrimSpace(spec.Method) == "":
		return errors.New("execute HTTP test request: method is required")
	case spec.JSON != nil && spec.Body != nil:
		return errors.New("execute HTTP test request: JSON and body are mutually exclusive")
	default:
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("execute HTTP test request: %w", cause)
		}
	}
	parsed, err := url.ParseRequestURI(spec.Path)
	if err != nil ||
		!strings.HasPrefix(spec.Path, "/") ||
		parsed.IsAbs() ||
		parsed.Host != "" {
		return errors.New(
			"execute HTTP test request: path must be a root-relative request target",
		)
	}
	return nil
}

func requestBody(spec HTTPRequest, maximum int64) ([]byte, error) {
	if spec.JSON == nil {
		if int64(len(spec.Body)) > maximum {
			return nil, fmt.Errorf(
				"encode HTTP test request: body exceeds %d bytes",
				maximum,
			)
		}
		return bytes.Clone(spec.Body), nil
	}
	content, err := json.Marshal(spec.JSON)
	if err != nil {
		return nil, fmt.Errorf("encode HTTP test request JSON: %w", err)
	}
	if int64(len(content)) > maximum {
		return nil, fmt.Errorf(
			"encode HTTP test request JSON: body exceeds %d bytes",
			maximum,
		)
	}
	return content, nil
}

func stopFailedApplication(
	application HTTPApplication,
	timeout time.Duration,
) error {
	if application == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := application.Stop(ctx); err != nil {
		return fmt.Errorf("stop failed HTTP test application: %w", err)
	}
	return nil
}

func wrapTestCloseError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
