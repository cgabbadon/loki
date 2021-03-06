// Copyright (c) 2012 The gocql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package gocql

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// interface to implement to receive the host information
type SetHosts interface {
	SetHosts(hosts []*HostInfo)
}

// interface to implement to receive the partitioner value
type SetPartitioner interface {
	SetPartitioner(partitioner string)
}

func setupTLSConfig(sslOpts *SslOptions) (*tls.Config, error) {
	if sslOpts.Config == nil {
		sslOpts.Config = &tls.Config{}
	}

	// ca cert is optional
	if sslOpts.CaPath != "" {
		if sslOpts.RootCAs == nil {
			sslOpts.RootCAs = x509.NewCertPool()
		}

		pem, err := ioutil.ReadFile(sslOpts.CaPath)
		if err != nil {
			return nil, fmt.Errorf("connectionpool: unable to open CA certs: %v", err)
		}

		if !sslOpts.RootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("connectionpool: failed parsing or CA certs")
		}
	}

	if sslOpts.CertPath != "" || sslOpts.KeyPath != "" {
		mycert, err := tls.LoadX509KeyPair(sslOpts.CertPath, sslOpts.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("connectionpool: unable to load X509 key pair: %v", err)
		}
		sslOpts.Certificates = append(sslOpts.Certificates, mycert)
	}

	sslOpts.InsecureSkipVerify = !sslOpts.EnableHostVerification

	// return clone to avoid race
	return sslOpts.Config.Clone(), nil
}

type policyConnPool struct {
	logger     log.Logger
	registerer prometheus.Registerer

	session *Session

	port     int
	numConns int
	keyspace string

	mu            sync.RWMutex
	hostConnPools map[string]*hostConnPool

	endpoints []string

	numHosts prometheus.GaugeFunc
}

func connConfig(cfg *ClusterConfig) (*ConnConfig, error) {
	var (
		err       error
		tlsConfig *tls.Config
	)

	// TODO(zariel): move tls config setup into session init.
	if cfg.SslOpts != nil {
		tlsConfig, err = setupTLSConfig(cfg.SslOpts)
		if err != nil {
			return nil, err
		}
	}

	return &ConnConfig{
		ProtoVersion:    cfg.ProtoVersion,
		CQLVersion:      cfg.CQLVersion,
		Timeout:         cfg.Timeout,
		ConnectTimeout:  cfg.ConnectTimeout,
		Dialer:          cfg.Dialer,
		Compressor:      cfg.Compressor,
		Authenticator:   cfg.Authenticator,
		AuthProvider:    cfg.AuthProvider,
		Keepalive:       cfg.SocketKeepalive,
		tlsConfig:       tlsConfig,
		disableCoalesce: tlsConfig != nil, // write coalescing doesn't work with framing on top of TCP like in TLS.
	}, nil
}

func newPolicyConnPool(logger log.Logger, registerer prometheus.Registerer, session *Session) *policyConnPool {
	// create the pool
	pool := &policyConnPool{
		logger:        logger,
		registerer:    registerer,
		session:       session,
		port:          session.cfg.Port,
		numConns:      session.cfg.NumConns,
		keyspace:      session.cfg.Keyspace,
		hostConnPools: map[string]*hostConnPool{},
	}

	pool.endpoints = make([]string, len(session.cfg.Hosts))
	copy(pool.endpoints, session.cfg.Hosts)

	pool.numHosts = promauto.With(registerer).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gocql_connection_pool_hosts",
		Help: "Current number of hosts in the connection pool.",
	}, func() float64 {
		pool.mu.RLock()
		defer pool.mu.RUnlock()
		return float64(len(pool.hostConnPools))
	})

	return pool
}

func (p *policyConnPool) SetHosts(hosts []*HostInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()

	toRemove := make(map[string]struct{})
	for addr := range p.hostConnPools {
		toRemove[addr] = struct{}{}
	}

	pools := make(chan *hostConnPool)
	createCount := 0
	for _, host := range hosts {
		if !host.IsUp() {
			// don't create a connection pool for a down host
			continue
		}
		ip := host.ConnectAddress().String()
		if _, exists := p.hostConnPools[ip]; exists {
			// still have this host, so don't remove it
			delete(toRemove, ip)
			continue
		}

		createCount++
		go func(host *HostInfo) {
			// create a connection pool for the host
			pools <- newHostConnPool(
				p.logger,
				p.registerer,
				p.session,
				host,
				p.port,
				p.numConns,
				p.keyspace,
			)
		}(host)
	}

	// add created pools
	for createCount > 0 {
		pool := <-pools
		createCount--
		if pool.Size() > 0 {
			// add pool only if there a connections available
			p.hostConnPools[string(pool.host.ConnectAddress())] = pool
		}
	}

	for addr := range toRemove {
		pool := p.hostConnPools[addr]
		pool.deregisterMetrics()
		delete(p.hostConnPools, addr)
		go pool.Close()
	}
}

func (p *policyConnPool) Size() int {
	p.mu.RLock()
	count := 0
	for _, pool := range p.hostConnPools {
		count += pool.Size()
	}
	p.mu.RUnlock()

	return count
}

func (p *policyConnPool) getPool(host *HostInfo) (pool *hostConnPool, ok bool) {
	ip := host.ConnectAddress().String()
	p.mu.RLock()
	pool, ok = p.hostConnPools[ip]
	p.mu.RUnlock()
	return
}

func (p *policyConnPool) Close() {
	if p.registerer != nil {
		p.registerer.Unregister(p.numHosts)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// close the pools
	for addr, pool := range p.hostConnPools {
		delete(p.hostConnPools, addr)
		pool.deregisterMetrics()
		pool.Close()
	}
}

func (p *policyConnPool) addHost(host *HostInfo) {
	ip := host.ConnectAddress().String()
	p.mu.Lock()
	pool, ok := p.hostConnPools[ip]
	if !ok {
		pool = newHostConnPool(
			p.logger,
			p.registerer,
			p.session,
			host,
			host.Port(), // TODO: if port == 0 use pool.port?
			p.numConns,
			p.keyspace,
		)

		p.hostConnPools[ip] = pool
	}
	p.mu.Unlock()

	pool.fill()
}

func (p *policyConnPool) removeHost(ip net.IP) {
	k := ip.String()
	p.mu.Lock()
	pool, ok := p.hostConnPools[k]
	if !ok {
		p.mu.Unlock()
		return
	}

	pool.deregisterMetrics()
	delete(p.hostConnPools, k)
	p.mu.Unlock()

	go pool.Close()
}

func (p *policyConnPool) hostUp(host *HostInfo) {
	// TODO(zariel): have a set of up hosts and down hosts, we can internally
	// detect down hosts, then try to reconnect to them.
	p.addHost(host)
}

func (p *policyConnPool) hostDown(ip net.IP) {
	// TODO(zariel): mark host as down so we can try to connect to it later, for
	// now just treat it has removed.
	p.removeHost(ip)
}

// hostConnPool is a connection pool for a single host.
// Connection selection is based on a provided ConnSelectionPolicy
type hostConnPool struct {
	logger     log.Logger
	registerer prometheus.Registerer

	session  *Session
	host     *HostInfo
	port     int
	addr     string
	size     int
	keyspace string
	// protection for conns, closed, filling
	mu      sync.RWMutex
	conns   []*Conn
	closed  bool
	filling bool

	pos uint32

	connections        prometheus.GaugeFunc
	connectionAttempts prometheus.Counter
	connectionFailures prometheus.Counter
	connectionDrops    prometheus.Counter
}

func (h *hostConnPool) String() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return fmt.Sprintf("[filling=%v closed=%v conns=%v size=%v host=%v]",
		h.filling, h.closed, len(h.conns), h.size, h.host)
}

func newHostConnPool(logger log.Logger, registerer prometheus.Registerer, session *Session, host *HostInfo, port, size int,
	keyspace string) *hostConnPool {

	pool := &hostConnPool{
		logger:     logger,
		registerer: prometheus.WrapRegistererWith(prometheus.Labels{"host": host.ConnectAddress().String()}, registerer),
		session:    session,
		host:       host,
		port:       port,
		addr:       (&net.TCPAddr{IP: host.ConnectAddress(), Port: host.Port()}).String(),
		size:       size,
		keyspace:   keyspace,
		conns:      make([]*Conn, 0, size),
		filling:    false,
		closed:     false,
	}

	pool.connections = promauto.With(pool.registerer).NewGaugeFunc(prometheus.GaugeOpts{
		Name: "gocql_connection_pool_connections",
		Help: "Number of TCP connections in the pool for given host",
	}, func() float64 {
		return float64(pool.Size())
	})
	pool.connectionAttempts = promauto.With(pool.registerer).NewCounter(prometheus.CounterOpts{
		Name: "gocql_connection_pool_connection_attempts_total",
		Help: "Number of TCP connection attempts for given host",
	})
	pool.connectionFailures = promauto.With(pool.registerer).NewCounter(prometheus.CounterOpts{
		Name: "gocql_connection_pool_connection_failures_total",
		Help: "Number of TCP connection failures for given host",
	})
	pool.connectionDrops = promauto.With(pool.registerer).NewCounter(prometheus.CounterOpts{
		Name: "gocql_connection_pool_connection_drops_total",
		Help: "Number of TCP connection drops for given host",
	})

	// the pool is not filled or connected
	return pool
}

func (pool *hostConnPool) deregisterMetrics() {
	pool.registerer.Unregister(pool.connections)
	pool.registerer.Unregister(pool.connectionAttempts)
	pool.registerer.Unregister(pool.connectionFailures)
	pool.registerer.Unregister(pool.connectionDrops)
}

// Pick a connection from this connection pool for the given query.
func (pool *hostConnPool) Pick() *Conn {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	if pool.closed {
		return nil
	}

	size := len(pool.conns)
	if size < pool.size {
		// try to fill the pool
		go pool.fill()

		if size == 0 {
			return nil
		}
	}

	pos := int(atomic.AddUint32(&pool.pos, 1) - 1)

	var (
		leastBusyConn    *Conn
		streamsAvailable int
	)

	// find the conn which has the most available streams, this is racy
	for i := 0; i < size; i++ {
		conn := pool.conns[(pos+i)%size]
		if streams := conn.AvailableStreams(); streams > streamsAvailable {
			leastBusyConn = conn
			streamsAvailable = streams
		}
	}

	return leastBusyConn
}

//Size returns the number of connections currently active in the pool
func (pool *hostConnPool) Size() int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return len(pool.conns)
}

//Close the connection pool
func (pool *hostConnPool) Close() {
	pool.mu.Lock()

	if pool.closed {
		pool.mu.Unlock()
		return
	}
	pool.closed = true

	// ensure we dont try to reacquire the lock in handleError
	// TODO: improve this as the following can happen
	// 1) we have locked pool.mu write lock
	// 2) conn.Close calls conn.closeWithError(nil)
	// 3) conn.closeWithError calls conn.Close() which returns an error
	// 4) conn.closeWithError calls pool.HandleError with the error from conn.Close
	// 5) pool.HandleError tries to lock pool.mu
	// deadlock

	// empty the pool
	conns := pool.conns
	pool.conns = nil

	pool.mu.Unlock()

	// close the connections
	for _, conn := range conns {
		conn.Close()
	}
}

// Fill the connection pool
func (pool *hostConnPool) fill() {
	pool.mu.RLock()
	// avoid filling a closed pool, or concurrent filling
	if pool.closed || pool.filling {
		pool.mu.RUnlock()
		return
	}

	// determine the filling work to be done
	startCount := len(pool.conns)
	fillCount := pool.size - startCount

	// avoid filling a full (or overfull) pool
	if fillCount <= 0 {
		pool.mu.RUnlock()
		return
	}

	// switch from read to write lock
	pool.mu.RUnlock()
	pool.mu.Lock()

	// double check everything since the lock was released
	startCount = len(pool.conns)
	fillCount = pool.size - startCount
	if pool.closed || pool.filling || fillCount <= 0 {
		// looks like another goroutine already beat this
		// goroutine to the filling
		pool.mu.Unlock()
		return
	}

	// ok fill the pool
	pool.filling = true

	// allow others to access the pool while filling
	pool.mu.Unlock()
	// only this goroutine should make calls to fill/empty the pool at this
	// point until after this routine or its subordinates calls
	// fillingStopped

	// fill only the first connection synchronously
	if startCount == 0 {
		err := pool.connect()
		pool.logConnectErr(err)

		if err != nil {
			// probably unreachable host
			pool.fillingStopped(true)

			// this is call with the connection pool mutex held, this call will
			// then recursively try to lock it again. FIXME
			if pool.session.cfg.ConvictionPolicy.AddFailure(err, pool.host) {
				go pool.session.handleNodeDown(pool.host.ConnectAddress(), pool.port)
			}
			return
		}

		// filled one
		fillCount--
	}

	// fill the rest of the pool asynchronously
	go func() {
		err := pool.connectMany(fillCount)

		// mark the end of filling
		pool.fillingStopped(err != nil)
	}()
}

func (pool *hostConnPool) logConnectErr(err error) {
	if opErr, ok := err.(*net.OpError); ok && (opErr.Op == "dial" || opErr.Op == "read") {
		// connection refused
		// these are typical during a node outage so avoid log spam.
		if gocqlDebug {
			level.Debug(pool.logger).Log("msg", "unable to dial", "address", pool.host.ConnectAddress(), "error", err)
		}
	} else if err != nil {
		// unexpected error
		level.Error(pool.logger).Log("msg", "failed to connect", "address", pool.addr, "error", err)
	}
}

// transition back to a not-filling state.
func (pool *hostConnPool) fillingStopped(hadError bool) {
	if hadError {
		// wait for some time to avoid back-to-back filling
		// this provides some time between failed attempts
		// to fill the pool for the host to recover
		time.Sleep(time.Duration(rand.Int31n(100)+31) * time.Millisecond)
	}

	pool.mu.Lock()
	pool.filling = false
	pool.mu.Unlock()
}

// connectMany creates new connections concurrent.
func (pool *hostConnPool) connectMany(count int) error {
	if count == 0 {
		return nil
	}
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		connectErr error
	)
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			err := pool.connect()
			pool.logConnectErr(err)
			if err != nil {
				mu.Lock()
				connectErr = err
				mu.Unlock()
			}
		}()
	}
	// wait for all connections are done
	wg.Wait()

	return connectErr
}

// create a new connection to the host and add it to the pool
func (pool *hostConnPool) connect() (err error) {
	pool.connectionAttempts.Inc()
	defer func() {
		if err != nil {
			pool.connectionFailures.Inc()
		}
	}()

	// TODO: provide a more robust connection retry mechanism, we should also
	// be able to detect hosts that come up by trying to connect to downed ones.
	// try to connect
	var conn *Conn
	reconnectionPolicy := pool.session.cfg.ReconnectionPolicy
	for i := 0; i < reconnectionPolicy.GetMaxRetries(); i++ {
		conn, err = pool.session.connect(pool.session.ctx, pool.logger, pool.host, pool)
		if err == nil {
			break
		}
		if opErr, isOpErr := err.(*net.OpError); isOpErr {
			// if the error is not a temporary error (ex: network unreachable) don't
			//  retry
			if !opErr.Temporary() {
				break
			}
		}
		if gocqlDebug {
			level.Debug(pool.logger).Log("msg", "connection failed", "address", pool.host.ConnectAddress(),
				"policy", reconnectionPolicy)
		}
		time.Sleep(reconnectionPolicy.GetInterval(i))
	}

	if err != nil {
		return err
	}

	if pool.keyspace != "" {
		// set the keyspace
		if err = conn.UseKeyspace(pool.keyspace); err != nil {
			conn.Close()
			return err
		}
	}

	// add the Conn to the pool
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if pool.closed {
		conn.Close()
		return nil
	}

	pool.conns = append(pool.conns, conn)

	return nil
}

// handle any error from a Conn
func (pool *hostConnPool) HandleError(conn *Conn, err error, closed bool) {
	level.Error(pool.logger).Log("msg", "hostConnPool.HandleError", "err", err, "closed", closed)

	if !closed {
		// still an open connection, so continue using it
		return
	}

	pool.connectionDrops.Inc()

	// TODO: track the number of errors per host and detect when a host is dead,
	// then also have something which can detect when a host comes back.
	pool.mu.Lock()
	defer pool.mu.Unlock()

	if pool.closed {
		// pool closed
		return
	}

	// find the connection index
	for i, candidate := range pool.conns {
		if candidate == conn {
			// remove the connection, not preserving order
			pool.conns[i], pool.conns = pool.conns[len(pool.conns)-1], pool.conns[:len(pool.conns)-1]

			// lost a connection, so fill the pool
			go pool.fill()
			break
		}
	}
}
