package server

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kuasar-sandbox/accelerator/internal/util/optrace"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
)

// WireServer accepts TCP/UDS connections and serves cache RPCs using the
// binary wire protocol.
type WireServer struct {
	handler     *CacheHandler
	listener    net.Listener
	wg          sync.WaitGroup
	closing     atomic.Bool
	idleTimeout time.Duration
	rpcTimeout  time.Duration
	tr          *optrace.Tracer

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// NewWireServer creates a wire-protocol server.
//
// idleTimeout controls how long an idle connection stays open between
// requests before being closed by the server (0 = no idle timeout).
//
// rpcTimeout is the per-request wall-clock budget passed into
// HandleFrame as a ctx deadline. Backends that honour context
// (tiered-mode remote hops, origin FS reads) will be cancelled when
// the deadline fires; backends that don't (rocks CGO) run to
// completion regardless but the timeout still bounds the wire server
// from issuing a response after the deadline expires. 0 = no per-
// request deadline.
func NewWireServer(handler *CacheHandler, idleTimeout, rpcTimeout time.Duration) *WireServer {
	return &WireServer{
		handler:     handler,
		idleTimeout: idleTimeout,
		rpcTimeout:  rpcTimeout,
		tr:          optrace.FromEnv("cache-ctl"),
		conns:       make(map[net.Conn]struct{}),
	}
}

// Serve accepts connections on lis until GracefulStop is called.
func (s *WireServer) Serve(lis net.Listener) error {
	s.listener = lis
	for {
		conn, err := lis.Accept()
		if err != nil {
			if s.closing.Load() {
				return nil
			}
			continue
		}
		go s.serveConn(conn)
	}
}

func (s *WireServer) trackConn(c net.Conn) bool {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	if s.closing.Load() {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *WireServer) untrackConn(c net.Conn) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	delete(s.conns, c)
}

// ConnCount returns the number of currently-tracked client connections, for the
// periodic stats line.
func (s *WireServer) ConnCount() int {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()
	return len(s.conns)
}

// Handler exposes the wire handler so the stats printer can read its counters.
func (s *WireServer) Handler() *CacheHandler { return s.handler }

func (s *WireServer) serveConn(c net.Conn) {
	if !s.trackConn(c) {
		c.Close()
		return
	}
	s.wg.Add(1)
	defer s.wg.Done()
	defer func() {
		s.untrackConn(c)
		c.Close()
	}()

	// TCP tuning.
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
		tc.SetNoDelay(true)
	}

	wc := wire.NewConn(c)

	// Reader goroutine: owns bufio.Reader exclusively. Reads frames
	// into reqs channel. Idle timeout implemented via SetReadDeadline.
	reqs := make(chan *wire.Request, 2)
	go func() {
		defer close(reqs)
		for {
			if s.idleTimeout > 0 {
				wc.SetReadDeadline(time.Now().Add(s.idleTimeout))
			}
			req, err := wc.ReadRequest()
			if err != nil {
				return
			}
			reqs <- req
		}
	}()

	var pendingReqs []*wire.Request

	for {
		var req *wire.Request
		if len(pendingReqs) > 0 {
			req = pendingReqs[0]
			pendingReqs = pendingReqs[1:]
		} else {
			var ok bool
			req, ok = <-reqs
			if !ok {
				return
			}
		}

		if req.Opcode == wire.OpcodePing {
			wc.WriteResponse(&wire.Response{Status: wire.StatusHit, Flags: s.handler.Capabilities()})
			req.Release()
			continue
		}
		if req.Opcode == wire.OpcodeCancelRequest {
			req.Release()
			continue
		}

		ctx := context.Background()
		var cancelHandler context.CancelFunc
		if s.rpcTimeout > 0 {
			ctx, cancelHandler = context.WithTimeout(ctx, s.rpcTimeout)
		} else {
			ctx, cancelHandler = context.WithCancel(ctx)
		}

		resps := make(chan *wire.Response, 1)
		go func() {
			end := s.tr.Begin("cache.req")
			r := s.handler.HandleFrame(ctx, req)
			end()
			resps <- r
		}()

		var cancelled bool
	inner:
		for {
			select {
			case nextReq, ok := <-reqs:
				if !ok {
					cancelHandler()
					resp := <-resps
					if resp.Value != nil {
						resp.Value.Release()
					}
					req.Release()
					return
				}
				if nextReq.Opcode == wire.OpcodeCancelRequest {
					nextReq.Release()
					cancelHandler()
					cancelled = true
				} else if nextReq.Opcode == wire.OpcodePing {
					wc.WriteResponse(&wire.Response{Status: wire.StatusHit, Flags: s.handler.Capabilities()})
					nextReq.Release()
				} else {
					pendingReqs = append(pendingReqs, nextReq)
				}

			case resp := <-resps:
				cancelHandler()
				if cancelled {
					if resp.Value != nil {
						resp.Value.Release()
					}
					wc.WriteResponse(&wire.Response{Status: wire.StatusCancelled})
					for _, pr := range pendingReqs {
						wc.WriteResponse(&wire.Response{Status: wire.StatusCancelled})
						pr.Release()
					}
					pendingReqs = pendingReqs[:0]
				} else {
					writeErr := wc.WriteResponse(resp)
					if resp.Value != nil {
						resp.Value.Release()
					}
					if writeErr != nil {
						req.Release()
						return
					}
				}
				req.Release()
				break inner
			}
		}
	}
}

// GracefulStop stops accepting new connections, closes existing idle
// connections, and waits for in-flight requests to complete (with a 5-second
// hard deadline).
func (s *WireServer) GracefulStop() {
	s.closing.Store(true)
	if s.listener != nil {
		s.listener.Close()
	}
	// Force existing connections to exit their read loops by closing them.
	// In-flight handlers will still finish writing the response because
	// serveConn uses defer c.Close() (close is idempotent).
	s.connsMu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.connsMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
