package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
)

// ErrCancelled is returned by GetShard/FillShard when the server
// responds with StatusCancelled (in response to a client CancelRequest).
var ErrCancelled = errors.New("client: request cancelled")

// ConnPoolConfig configures a ConnPool.
type ConnPoolConfig struct {
	PoolSize int           // pre-established connections (0 = on-demand only)
	MaxSize  int           // max total connections including burst (must be >= PoolSize)
	Timeout  time.Duration // per-op TCP deadline
	IdleMax  time.Duration // burst connections idle this long get reaped (0 = no reaping)
}

// ConnPool is a channel-based connection pool with elastic burst
// capacity. Pre-established connections are dialled at creation;
// burst connections are dialled on-demand when all pre-established
// conns are busy and current < MaxSize. Excess burst connections
// are reaped after IdleMax of idleness.
type ConnPool struct {
	free    chan *PoolConn
	addr    string
	network string
	config  ConnPoolConfig
	current atomic.Int32 // total alive connections (free + in-use)
	closed  atomic.Bool
	done    chan struct{}
}

// PoolConn wraps a single wire.Conn owned by a ConnPool.
type PoolConn struct {
	conn *wire.Conn
	pool *ConnPool
}

// DialConnPool creates a pool with cfg.PoolSize pre-established
// connections. Burst connections up to cfg.MaxSize are dialled
// on-demand by Acquire when all pre-established conns are busy.
func DialConnPool(addr string, cfg ConnPoolConfig) (*ConnPool, error) {
	if cfg.MaxSize < cfg.PoolSize {
		cfg.MaxSize = cfg.PoolSize
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 1
	}
	// cfg.Timeout <= 0 stays 0 = no per-op deadline (SetOpDeadline
	// no-ops on <=0, so ops are bounded only by the caller's context).
	// The TCP/UDS *connect* still keeps a bounded fallback below — a
	// dead listener must not hang the dial forever.
	network, dialAddr := parseAddr(addr)

	p := &ConnPool{
		free:    make(chan *PoolConn, cfg.MaxSize),
		addr:    dialAddr,
		network: network,
		config:  cfg,
		done:    make(chan struct{}),
	}

	dialTimeout := cfg.Timeout
	if dialTimeout <= 0 {
		dialTimeout = 2 * time.Second // bounded connect even when ops are unbounded
	}
	for i := 0; i < cfg.PoolSize; i++ {
		raw, err := net.DialTimeout(network, dialAddr, dialTimeout)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("dial %s: %w", addr, err)
		}
		tcpTune(raw)
		p.free <- &PoolConn{conn: wire.NewConn(raw), pool: p}
		p.current.Add(1)
	}

	if cfg.PoolSize > 0 {
		go p.healthLoop()
	}
	if cfg.IdleMax > 0 {
		go p.reaperLoop()
	}

	return p, nil
}

const (
	pingInterval = 10 * time.Second
	pingTimeout  = 2 * time.Second
)

func (p *ConnPool) healthLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
		}
		select {
		case pc := <-p.free:
			if err := pc.Ping(pingTimeout); err != nil {
				pc.closeConn()
				p.current.Add(-1)
				if int(p.current.Load()) < p.config.PoolSize {
					go p.refill()
				}
			} else {
				p.free <- pc
			}
		default:
		}
	}
}

func (p *ConnPool) reaperLoop() {
	ticker := time.NewTicker(p.config.IdleMax)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
		}
		for int(p.current.Load()) > p.config.PoolSize {
			select {
			case pc := <-p.free:
				pc.closeConn()
				p.current.Add(-1)
			default:
				goto done
			}
		}
	done:
	}
}

// Acquire returns an idle connection, dialling a burst connection if
// needed and allowed by MaxSize. Blocks if at capacity until a
// connection is released or ctx expires.
func (p *ConnPool) Acquire(ctx context.Context) (*PoolConn, error) {
	// Fast path: take from free.
	select {
	case pc := <-p.free:
		return pc, nil
	default:
	}

	// Try burst dial.
	if n := p.current.Add(1); int(n) <= p.config.MaxSize {
		raw, err := net.DialTimeout(p.network, p.addr, p.config.Timeout)
		if err == nil {
			tcpTune(raw)
			return &PoolConn{conn: wire.NewConn(raw), pool: p}, nil
		}
		p.current.Add(-1)
	} else {
		p.current.Add(-1)
	}

	// Block until available or ctx done.
	select {
	case pc := <-p.free:
		return pc, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Release returns a connection to the pool.
func (p *ConnPool) Release(pc *PoolConn, healthy bool) {
	if p.closed.Load() {
		pc.closeConn()
		p.current.Add(-1)
		return
	}
	if !healthy {
		pc.closeConn()
		p.current.Add(-1)
		if int(p.current.Load()) < p.config.PoolSize {
			go p.refill()
		}
		return
	}
	select {
	case p.free <- pc:
	default:
		pc.closeConn()
		p.current.Add(-1)
	}
}

// Close shuts down the pool.
func (p *ConnPool) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(p.done)
	var firstErr error
	for {
		select {
		case pc := <-p.free:
			if err := pc.closeConn(); err != nil && firstErr == nil {
				firstErr = err
			}
			p.current.Add(-1)
		default:
			return firstErr
		}
	}
}

func (p *ConnPool) refill() {
	backoff := 500 * time.Millisecond
	const maxRetries = 5
	for i := 0; i < maxRetries; i++ {
		if p.closed.Load() {
			return
		}
		raw, err := net.DialTimeout(p.network, p.addr, p.config.Timeout)
		if err == nil {
			tcpTune(raw)
			pc := &PoolConn{conn: wire.NewConn(raw), pool: p}
			p.current.Add(1)
			select {
			case p.free <- pc:
			default:
				pc.closeConn()
				p.current.Add(-1)
			}
			return
		}
		log.Printf("conn_pool: refill %s failed (%d/%d): %v", p.addr, i+1, maxRetries, err)
		time.Sleep(backoff)
		backoff *= 2
	}
}

// PoolConn methods

func (pc *PoolConn) WriteRequest(req *wire.Request) error {
	return pc.conn.WriteRequest(req)
}

func (pc *PoolConn) ReadResponse(pool cache.BlobPool) (*wire.Response, error) {
	return pc.conn.ReadResponse(pool)
}

func (pc *PoolConn) SendCancel() error {
	return pc.conn.SendCancel()
}

func (pc *PoolConn) Drain(pool cache.BlobPool) {
	resp, err := pc.conn.ReadResponse(pool)
	if err != nil {
		return
	}
	if resp.Value != nil {
		resp.Value.Release()
	}
}

func (pc *PoolConn) Ping(timeout time.Duration) error {
	_, err := pc.PingCapabilities(timeout)
	return err
}

// PingCapabilities performs the normal health ping and returns protocol
// capabilities carried in the response's formerly-reserved flags byte.
func (pc *PoolConn) PingCapabilities(timeout time.Duration) (byte, error) {
	_ = pc.conn.SetDeadline(time.Now().Add(timeout))
	defer pc.conn.SetDeadline(time.Time{})
	if err := pc.conn.WriteRequest(&wire.Request{Opcode: wire.OpcodePing}); err != nil {
		return 0, err
	}
	resp, err := pc.conn.ReadResponse(nil)
	if err != nil {
		return 0, err
	}
	if resp.Value != nil {
		resp.Value.Release()
	}
	if resp.Status != wire.StatusHit {
		return 0, fmt.Errorf("cache: ping status %d", resp.Status)
	}
	return resp.Flags, nil
}

func (pc *PoolConn) closeConn() error {
	if pc.conn != nil {
		return pc.conn.Close()
	}
	return nil
}

// SetOpDeadline sets read+write deadlines to the earlier of
// (now + timeout) and the ctx deadline. Honouring ctx is essential:
// without it, an upstream hop could keep blocking on a TCP read long
// after the caller's context has expired — and in a TieredCache chain
// that latency propagates to the external client's own timeout, which
// then races the intermediate hop's conn timeout.
//
// Uses a single net.Conn.SetDeadline syscall (covers read + write
// atomically) rather than separate SetReadDeadline + SetWriteDeadline
// (2 syscalls). Per-shard GetShard calls 10 deadline syscalls per Get
// under the old scheme; with this, it's 5.
//
// Pass ctx=nil to use only the timeout budget (legacy behaviour).
func (pc *PoolConn) SetOpDeadline(ctx context.Context, timeout time.Duration) {
	if pc.conn == nil {
		return
	}
	var dl time.Time
	if timeout > 0 {
		dl = time.Now().Add(timeout)
	}
	if ctx != nil {
		if ctxDL, ok := ctx.Deadline(); ok {
			if dl.IsZero() || ctxDL.Before(dl) {
				dl = ctxDL
			}
		}
	}
	if dl.IsZero() {
		return
	}
	_ = pc.conn.SetDeadline(dl)
}

// Helpers

func parseAddr(addr string) (network, dialAddr string) {
	if strings.HasPrefix(addr, "unix://") {
		return "unix", strings.TrimPrefix(addr, "unix://")
	}
	if strings.HasPrefix(addr, "/") {
		return "unix", addr
	}
	return "tcp", addr
}

func tcpTune(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
	}
}
