// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudsqlconn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/cloudsqlconn/debug"
	"cloud.google.com/go/cloudsqlconn/errtype"
	"cloud.google.com/go/cloudsqlconn/instance"
	"cloud.google.com/go/cloudsqlconn/internal/cloudsql"
	"cloud.google.com/go/cloudsqlconn/internal/trace"
	"github.com/google/uuid"
	"golang.org/x/net/proxy"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1beta4"
)

const (
	// defaultTCPKeepAlive is the default keep alive value used on connections to a Cloud SQL instance.
	defaultTCPKeepAlive = 30 * time.Second
	// serverProxyPort is the port the server-side proxy receives connections on.
	serverProxyPort = "3307"
	// iamLoginScope is the OAuth2 scope used for tokens embedded in the ephemeral
	// certificate.
	iamLoginScope = "https://www.googleapis.com/auth/sqlservice.login"
)

var (
	// ErrDialerClosed is used when a caller invokes Dial after closing the
	// Dialer.
	ErrDialerClosed = errors.New("cloudsqlconn: dialer is closed")
	// versionString indicates the version of this library.
	//go:embed version.txt
	versionString string
	userAgent     = "cloud-sql-go-connector/" + strings.TrimSpace(versionString)

	// defaultKey is the default RSA public/private keypair used by the clients.
	defaultKey    *rsa.PrivateKey
	defaultKeyErr error
	keyOnce       sync.Once
)

func getDefaultKeys() (*rsa.PrivateKey, error) {
	keyOnce.Do(func() {
		defaultKey, defaultKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	return defaultKey, defaultKeyErr
}

type connectionInfoCache interface {
	ConnectionInfo(context.Context) (cloudsql.ConnectionInfo, error)
	UpdateRefresh(*bool)
	ForceRefresh()
	io.Closer
}

// monitoredCache is a wrapper around a connectionInfoCache that tracks the
// number of connections to the associated instance.
type monitoredCache struct {
	openConns uint64

	connectionInfoCache
}

// A Dialer is used to create connections to Cloud SQL instances.
//
// Use NewDialer to initialize a Dialer.
type Dialer struct {
	lock           sync.RWMutex
	cache          map[instance.ConnName]monitoredCache
	key            *rsa.PrivateKey
	refreshTimeout time.Duration
	// closed reports if the dialer has been closed.
	closed chan struct{}

	sqladmin *sqladmin.Service
	logger   debug.Logger

	// lazyRefresh determines what kind of caching is used for ephemeral
	// certificates. When lazyRefresh is true, the dialer will use a lazy
	// cache, refresh certificates only when a connection attempt needs a fresh
	// certificate. Otherwise, a refresh ahead cache will be used. The refresh
	// ahead cache assumes a background goroutine may run consistently.
	lazyRefresh bool

	// defaultDialConfig holds the constructor level DialOptions, so that it
	// can be copied and mutated by the Dial function.
	defaultDialConfig dialConfig

	// dialerID uniquely identifies a Dialer. Used for monitoring purposes,
	// *only* when a client has configured OpenCensus exporters.
	dialerID string

	// dialFunc is the function used to connect to the address on the named
	// network. By default, it is golang.org/x/net/proxy#Dial.
	dialFunc func(cxt context.Context, network, addr string) (net.Conn, error)

	// iamTokenSource supplies the OAuth2 token used for IAM DB Authn.
	iamTokenSource oauth2.TokenSource
}

var (
	errUseTokenSource    = errors.New("use WithTokenSource when IAM AuthN is not enabled")
	errUseIAMTokenSource = errors.New("use WithIAMAuthNTokenSources instead of WithTokenSource be used when IAM AuthN is enabled")
)

type nullLogger struct{}

func (nullLogger) Debugf(_ string, _ ...interface{}) {}

// NewDialer creates a new Dialer.
//
// Initial calls to NewDialer make take longer than normal because generation of an
// RSA keypair is performed. Calls with a WithRSAKeyPair DialOption or after a default
// RSA keypair is generated will be faster.
func NewDialer(ctx context.Context, opts ...Option) (*Dialer, error) {
	cfg := &dialerConfig{
		refreshTimeout:  cloudsql.RefreshTimeout,
		dialFunc:        proxy.Dial,
		logger:          nullLogger{},
		useragents:      []string{userAgent},
		serviceUniverse: "googleapis.com",
	}
	for _, opt := range opts {
		opt(cfg)
		if cfg.err != nil {
			return nil, cfg.err
		}
	}
	if cfg.useIAMAuthN && cfg.setTokenSource && !cfg.setIAMAuthNTokenSource {
		return nil, errUseIAMTokenSource
	}
	if cfg.setIAMAuthNTokenSource && !cfg.useIAMAuthN {
		return nil, errUseTokenSource
	}
	// Add this to the end to make sure it's not overridden
	cfg.sqladminOpts = append(cfg.sqladminOpts, option.WithUserAgent(strings.Join(cfg.useragents, " ")))

	// If callers have not provided a token source, either explicitly with
	// WithTokenSource or implicitly with WithCredentialsJSON etc., then use the
	// default token source.
	if !cfg.setCredentials {
		c, err := google.FindDefaultCredentials(ctx, sqladmin.SqlserviceAdminScope)
		if err != nil {
			return nil, fmt.Errorf("failed to create default credentials: %v", err)
		}
		ud, err := c.GetUniverseDomain()
		if err != nil {
			return nil, fmt.Errorf("failed to get universe domain: %v", err)
		}
		cfg.credentialsUniverse = ud
		cfg.sqladminOpts = append(cfg.sqladminOpts, option.WithTokenSource(c.TokenSource))
		scoped, err := google.DefaultTokenSource(ctx, iamLoginScope)
		if err != nil {
			return nil, fmt.Errorf("failed to create scoped token source: %v", err)
		}
		cfg.iamLoginTokenSource = scoped
	}

	if cfg.rsaKey == nil {
		key, err := getDefaultKeys()
		if err != nil {
			return nil, fmt.Errorf("failed to generate RSA keys: %v", err)
		}
		cfg.rsaKey = key
	}

	if cfg.setUniverseDomain && cfg.setAdminAPIEndpoint {
		return nil, errors.New(
			"can not use WithAdminAPIEndpoint and WithUniverseDomain Options together, " +
				"use WithAdminAPIEndpoint (it already contains the universe domain)",
		)
	}

	if cfg.credentialsUniverse != "" && cfg.serviceUniverse != "" {
		if cfg.credentialsUniverse != cfg.serviceUniverse {
			return nil, fmt.Errorf(
				"the configured service universe domain (%s) does not match the credential universe domain (%s)",
				cfg.serviceUniverse, cfg.credentialsUniverse,
			)
		}
	}

	client, err := sqladmin.NewService(ctx, cfg.sqladminOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create sqladmin client: %v", err)
	}

	dc := dialConfig{
		ipType:       cloudsql.PublicIP,
		tcpKeepAlive: defaultTCPKeepAlive,
		useIAMAuthN:  cfg.useIAMAuthN,
	}
	for _, opt := range cfg.dialOpts {
		opt(&dc)
	}

	if err := trace.InitMetrics(); err != nil {
		return nil, err
	}
	d := &Dialer{
		closed:            make(chan struct{}),
		cache:             make(map[instance.ConnName]monitoredCache),
		lazyRefresh:       cfg.lazyRefresh,
		key:               cfg.rsaKey,
		refreshTimeout:    cfg.refreshTimeout,
		sqladmin:          client,
		logger:            cfg.logger,
		defaultDialConfig: dc,
		dialerID:          uuid.New().String(),
		iamTokenSource:    cfg.iamLoginTokenSource,
		dialFunc:          cfg.dialFunc,
	}
	return d, nil
}

// Dial returns a net.Conn connected to the specified Cloud SQL instance. The
// icn argument must be the instance's connection name, which is in the format
// "project-name:region:instance-name".
func (d *Dialer) Dial(ctx context.Context, icn string, opts ...DialOption) (conn net.Conn, err error) {
	select {
	case <-d.closed:
		return nil, ErrDialerClosed
	default:
	}
	startTime := time.Now()
	var endDial trace.EndSpanFunc
	ctx, endDial = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn.Dial",
		trace.AddInstanceName(icn),
		trace.AddDialerID(d.dialerID),
	)
	defer func() {
		go trace.RecordDialError(context.Background(), icn, d.dialerID, err)
		endDial(err)
	}()
	cn, err := instance.ParseConnName(icn)
	if err != nil {
		return nil, err
	}

	cfg := d.defaultDialConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	var endInfo trace.EndSpanFunc
	ctx, endInfo = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.InstanceInfo")
	c := d.connectionInfoCache(cn, &cfg.useIAMAuthN)
	ci, err := c.ConnectionInfo(ctx)
	if err != nil {
		d.lock.Lock()
		defer d.lock.Unlock()
		d.logger.Debugf("[%v] Removing connection info from cache", cn.String())
		// Stop all background refreshes
		c.Close()
		delete(d.cache, cn)
		endInfo(err)
		return nil, err
	}
	endInfo(err)

	// If the client certificate has expired (as when the computer goes to
	// sleep, and the refresh cycle cannot run), force a refresh immediately.
	// The TLS handshake will not fail on an expired client certificate. It's
	// not until the first read where the client cert error will be surfaced.
	// So check that the certificate is valid before proceeding.
	if !validClientCert(cn, d.logger, ci.Expiration) {
		d.logger.Debugf("[%v] Refreshing certificate now", cn.String())
		c.ForceRefresh()
		// Block on refreshed connection info
		ci, err = c.ConnectionInfo(ctx)
		if err != nil {
			d.lock.Lock()
			defer d.lock.Unlock()
			d.logger.Debugf("[%v] Removing connection info from cache", cn.String())
			// Stop all background refreshes
			c.Close()
			delete(d.cache, cn)
			return nil, err
		}
	}

	var connectEnd trace.EndSpanFunc
	ctx, connectEnd = trace.StartSpan(ctx, "cloud.google.com/go/cloudsqlconn/internal.Connect")
	defer func() { connectEnd(err) }()
	addr, err := ci.Addr(cfg.ipType)
	if err != nil {
		return nil, err
	}
	addr = net.JoinHostPort(addr, serverProxyPort)
	f := d.dialFunc
	if cfg.dialFunc != nil {
		f = cfg.dialFunc
	}
	d.logger.Debugf("[%v] Dialing %v", cn.String(), addr)
	conn, err = f(ctx, "tcp", addr)
	if err != nil {
		d.logger.Debugf("[%v] Dialing %v failed: %v", cn.String(), addr, err)
		// refresh the instance info in case it caused the connection failure
		c.ForceRefresh()
		return nil, errtype.NewDialError("failed to dial", cn.String(), err)
	}
	if c, ok := conn.(*net.TCPConn); ok {
		if err := c.SetKeepAlive(true); err != nil {
			return nil, errtype.NewDialError("failed to set keep-alive", cn.String(), err)
		}
		if err := c.SetKeepAlivePeriod(cfg.tcpKeepAlive); err != nil {
			return nil, errtype.NewDialError("failed to set keep-alive period", cn.String(), err)
		}
	}

	tlsConn := tls.Client(conn, ci.TLSConfig())
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		d.logger.Debugf("[%v] TLS handshake failed: %v", cn.String(), err)
		// refresh the instance info in case it caused the handshake failure
		c.ForceRefresh()
		_ = tlsConn.Close() // best effort close attempt
		return nil, errtype.NewDialError("handshake failed", cn.String(), err)
	}

	latency := time.Since(startTime).Milliseconds()
	go func() {
		n := atomic.AddUint64(&c.openConns, 1)
		trace.RecordOpenConnections(ctx, int64(n), d.dialerID, cn.String())
		trace.RecordDialLatency(ctx, icn, d.dialerID, latency)
	}()

	return newInstrumentedConn(tlsConn, func() {
		n := atomic.AddUint64(&c.openConns, ^uint64(0))
		trace.RecordOpenConnections(context.Background(), int64(n), d.dialerID, cn.String())
	}), nil
}

// validClientCert checks that the ephemeral client certificate retrieved from
// the cache is unexpired. The time comparisons strip the monotonic clock value
// to ensure an accurate result, even after laptop sleep.
func validClientCert(cn instance.ConnName, l debug.Logger, expiration time.Time) bool {
	// Use UTC() to strip monotonic clock value to guard against inaccurate
	// comparisons, especially after laptop sleep.
	// See the comments on the monotonic clock in the Go documentation for
	// details: https://pkg.go.dev/time#hdr-Monotonic_Clocks
	now := time.Now().UTC()
	valid := expiration.UTC().After(now)
	l.Debugf(
		"[%v] Now = %v, Current cert expiration = %v",
		cn.String(),
		now.Format(time.RFC3339),
		expiration.UTC().Format(time.RFC3339),
	)
	l.Debugf("[%v] Cert is valid = %v", cn.String(), valid)
	return valid
}

// EngineVersion returns the engine type and version for the instance
// connection name. The value will correspond to one of the following types for
// the instance:
// https://cloud.google.com/sql/docs/mysql/admin-api/rest/v1beta4/SqlDatabaseVersion
func (d *Dialer) EngineVersion(ctx context.Context, icn string) (string, error) {
	cn, err := instance.ParseConnName(icn)
	if err != nil {
		return "", err
	}
	i := d.connectionInfoCache(cn, nil)
	ci, err := i.ConnectionInfo(ctx)
	if err != nil {
		return "", err
	}
	return ci.DBVersion, nil
}

// Warmup starts the background refresh necessary to connect to the instance.
// Use Warmup to start the refresh process early if you don't know when you'll
// need to call "Dial".
func (d *Dialer) Warmup(_ context.Context, icn string, opts ...DialOption) error {
	cn, err := instance.ParseConnName(icn)
	if err != nil {
		return err
	}
	cfg := d.defaultDialConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	_ = d.connectionInfoCache(cn, &cfg.useIAMAuthN)
	return nil
}

// newInstrumentedConn initializes an instrumentedConn that on closing will
// decrement the number of open connects and record the result.
func newInstrumentedConn(conn net.Conn, closeFunc func()) *instrumentedConn {
	return &instrumentedConn{
		Conn:      conn,
		closeFunc: closeFunc,
	}
}

// instrumentedConn wraps a net.Conn and invokes closeFunc when the connection
// is closed.
type instrumentedConn struct {
	net.Conn
	closeFunc func()
}

// Close delegates to the underlying net.Conn interface and reports the close
// to the provided closeFunc only when Close returns no error.
func (i *instrumentedConn) Close() error {
	err := i.Conn.Close()
	if err != nil {
		return err
	}
	go i.closeFunc()
	return nil
}

// Close closes the Dialer; it prevents the Dialer from refreshing the information
// needed to connect. Additional dial operations may succeed until the information
// expires.
func (d *Dialer) Close() error {
	// Check if Close has already been called.
	select {
	case <-d.closed:
		return nil
	default:
	}
	close(d.closed)
	d.lock.Lock()
	defer d.lock.Unlock()
	for _, i := range d.cache {
		i.Close()
	}
	return nil
}

// connectionInfoCache is a helper function for returning the appropriate
// connection info Cache in a threadsafe way. It will create a new cache,
// modify the existing one, or leave it unchanged as needed.
func (d *Dialer) connectionInfoCache(
	cn instance.ConnName, useIAMAuthN *bool,
) monitoredCache {
	d.lock.RLock()
	c, ok := d.cache[cn]
	d.lock.RUnlock()
	if !ok {
		d.lock.Lock()
		defer d.lock.Unlock()
		// Recheck to ensure instance wasn't created or changed between locks
		c, ok = d.cache[cn]
		if !ok {
			var useIAMAuthNDial bool
			if useIAMAuthN != nil {
				useIAMAuthNDial = *useIAMAuthN
			}
			d.logger.Debugf("[%v] Connection info added to cache", cn.String())
			var cache connectionInfoCache
			if d.lazyRefresh {
				cache = cloudsql.NewLazyRefreshCache(
					cn,
					d.logger,
					d.sqladmin, d.key,
					d.refreshTimeout, d.iamTokenSource,
					d.dialerID, useIAMAuthNDial,
				)
			} else {
				cache = cloudsql.NewRefreshAheadCache(
					cn,
					d.logger,
					d.sqladmin, d.key,
					d.refreshTimeout, d.iamTokenSource,
					d.dialerID, useIAMAuthNDial,
				)
			}
			c = monitoredCache{connectionInfoCache: cache}
			d.cache[cn] = c
		}
	}

	c.UpdateRefresh(useIAMAuthN)

	return c
}
