package wire

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
)

// ---------------------------------------------------------------------------
// Request round-trip
// ---------------------------------------------------------------------------

func TestRequestRoundTrip_ObjectGet(t *testing.T) {
	req := &Request{
		Opcode:    OpcodeObjectGet,
		Namespace: NSChunk,
		Hash:      sha256Hash("hello"),
	}
	got := roundTripRequest(t, req)
	assertRequestEqual(t, req, got)
	if got.Value != nil {
		t.Fatal("Get request should have nil Value")
	}
}

func TestRequestRoundTrip_ObjectPut(t *testing.T) {
	value := make([]byte, 512*1024) // 512 KB
	rand.Read(value)
	req := &Request{
		Opcode:    OpcodeObjectPut,
		Namespace: NSManifest,
		Hash:      sha256Hash("world"),
		Value:     value,
	}
	got := roundTripRequest(t, req)
	assertRequestEqual(t, req, got)
	if !bytes.Equal(got.Value, value) {
		t.Fatal("Put value mismatch")
	}
	got.Release() // exercise pool return
}

func TestRequestRoundTrip_ShardGet(t *testing.T) {
	req := &Request{
		Opcode:    OpcodeShardGet,
		Namespace: NSChunk,
		Hash:      sha256Hash("shard"),
	}
	got := roundTripRequest(t, req)
	assertRequestEqual(t, req, got)
}

func TestRequestRoundTrip_ShardPut(t *testing.T) {
	// Value carries [idx][total][data] prefix on the wire; the wire
	// codec treats the whole payload as opaque bytes.
	value := append([]byte{4, 5}, []byte("shard-data")...)
	req := &Request{
		Opcode:    OpcodeShardPut,
		Namespace: NSChunk,
		Hash:      sha256Hash("shard-put"),
		Value:     value,
	}
	got := roundTripRequest(t, req)
	assertRequestEqual(t, req, got)
	if !bytes.Equal(got.Value, value) {
		t.Fatal("Shard put value mismatch")
	}
	got.Release()
}

func TestRequestRoundTrip_Ping(t *testing.T) {
	req := &Request{Opcode: OpcodePing}
	got := roundTripRequest(t, req)
	if got.Opcode != OpcodePing {
		t.Fatalf("Opcode: got %d, want %d", got.Opcode, OpcodePing)
	}
}

func TestResponseFlagsRoundTrip(t *testing.T) {
	want := &Response{Status: StatusHit, Flags: CapabilityWarmOnly}
	var buf bytes.Buffer
	if err := WriteResponse(&buf, want); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	got, err := ReadResponse(&buf, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if got.Flags != CapabilityWarmOnly {
		t.Fatalf("flags=%#x, want %#x", got.Flags, CapabilityWarmOnly)
	}
}

// ---------------------------------------------------------------------------
// Response round-trip
// ---------------------------------------------------------------------------

func TestResponseRoundTrip_Hit(t *testing.T) {
	value := make([]byte, 512*1024)
	rand.Read(value)
	resp := &Response{
		Status: StatusHit,
		Value:  cache.NewMemBlob(value),
	}
	got := roundTripResponse(t, resp)
	if got.Status != StatusHit {
		t.Fatalf("Status: got %d, want %d", got.Status, StatusHit)
	}
	if !bytes.Equal(got.Value.Bytes(), value) {
		t.Fatal("Hit value mismatch")
	}
	got.Value.Release()
}

func TestResponseRoundTrip_Miss(t *testing.T) {
	resp := &Response{Status: StatusMiss}
	got := roundTripResponse(t, resp)
	if got.Status != StatusMiss {
		t.Fatalf("Status: got %d, want %d", got.Status, StatusMiss)
	}
	if got.Value != nil {
		t.Fatal("Miss should have nil Value")
	}
}

func TestResponseRoundTrip_Error(t *testing.T) {
	resp := &Response{
		Status: StatusError,
		ErrMsg: "rocks: get: corruption",
	}
	got := roundTripResponse(t, resp)
	if got.Status != StatusError {
		t.Fatalf("Status: got %d, want %d", got.Status, StatusError)
	}
	if got.ErrMsg != resp.ErrMsg {
		t.Fatalf("ErrMsg: got %q, want %q", got.ErrMsg, resp.ErrMsg)
	}
}

// ---------------------------------------------------------------------------
// Boundary / error cases
// ---------------------------------------------------------------------------

func TestRequestMinimalFrame(t *testing.T) {
	// TotalLen == RequestHeaderSize (40), no value.
	req := &Request{Opcode: OpcodeObjectGet, Hash: sha256Hash("min")}
	got := roundTripRequest(t, req)
	assertRequestEqual(t, req, got)
}

func TestResponseMinimalFrame(t *testing.T) {
	// TotalLen == ResponseHeaderSize (8), MISS.
	resp := &Response{Status: StatusMiss}
	got := roundTripResponse(t, resp)
	if got.Status != StatusMiss {
		t.Fatalf("Status: got %d, want %d", got.Status, StatusMiss)
	}
}

func TestReadRequest_FrameTooLarge(t *testing.T) {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], MaxFrameSize+1)
	_, err := ReadRequest(bytes.NewReader(hdr[:]))
	if err != ErrFrameTooLarge {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestReadRequest_FrameTooSmall(t *testing.T) {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], 10) // less than 40
	_, err := ReadRequest(bytes.NewReader(hdr[:]))
	if err != ErrFrameTooSmall {
		t.Fatalf("expected ErrFrameTooSmall, got %v", err)
	}
}

func TestReadResponse_FrameTooLarge(t *testing.T) {
	var hdr [ResponseHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[:4], MaxFrameSize+1)
	_, err := ReadResponse(bytes.NewReader(hdr[:]), nil)
	if err != ErrFrameTooLarge {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestReadResponse_FrameTooSmall(t *testing.T) {
	var hdr [ResponseHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[:4], 4) // less than 8
	_, err := ReadResponse(bytes.NewReader(hdr[:]), nil)
	if err != ErrFrameTooSmall {
		t.Fatalf("expected ErrFrameTooSmall, got %v", err)
	}
}

func TestReadRequest_Truncated(t *testing.T) {
	// Write a valid header that promises 100 bytes of value, but provide none.
	var hdr [RequestHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[:4], RequestHeaderSize+100)
	hdr[4] = OpcodeObjectPut
	_, err := ReadRequest(bytes.NewReader(hdr[:]))
	if err == nil {
		t.Fatal("expected error on truncated frame")
	}
}

func TestReadResponse_Truncated(t *testing.T) {
	var hdr [ResponseHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[:4], ResponseHeaderSize+100)
	hdr[4] = StatusHit
	_, err := ReadResponse(bytes.NewReader(hdr[:]), nil)
	if err == nil {
		t.Fatal("expected error on truncated frame")
	}
}

// ---------------------------------------------------------------------------
// Conn over a real TCP pipe
// ---------------------------------------------------------------------------

func TestConnRoundTrip(t *testing.T) {
	srv, cli := tcpPipe(t)
	defer srv.Close()
	defer cli.Close()

	srvConn := NewConn(srv)
	cliConn := NewConn(cli)

	value := make([]byte, 256*1024)
	rand.Read(value)

	// Client writes request, server reads it.
	done := make(chan error, 1)
	go func() {
		req, err := srvConn.ReadRequest()
		if err != nil {
			done <- err
			return
		}
		// Server writes response.
		resp := &Response{Status: StatusHit, Value: cache.NewMemBlob(value)}
		done <- srvConn.WriteResponse(resp)
		resp.Value.Release()
		req.Release()
	}()

	req := &Request{
		Opcode:    OpcodeObjectGet,
		Namespace: NSChunk,
		Hash:      sha256Hash("tcp-test"),
	}
	if err := cliConn.WriteRequest(req); err != nil {
		t.Fatal(err)
	}

	resp, err := cliConn.ReadResponse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusHit {
		t.Fatalf("Status: got %d, want HIT", resp.Status)
	}
	if !bytes.Equal(resp.Value.Bytes(), value) {
		t.Fatal("value mismatch over TCP")
	}
	resp.Value.Release()

	if err := <-done; err != nil {
		t.Fatalf("server goroutine: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sha256Hash(s string) [32]byte {
	var h [32]byte
	copy(h[:], []byte(s))
	return h
}

func roundTripRequest(t *testing.T, req *Request) *Request {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatalf("WriteRequest: %v", err)
	}
	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatalf("ReadRequest: %v", err)
	}
	return got
}

func roundTripResponse(t *testing.T, resp *Response) *Response {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteResponse(&buf, resp); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	got, err := ReadResponse(&buf, nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	return got
}

func assertRequestEqual(t *testing.T, want, got *Request) {
	t.Helper()
	if got.Opcode != want.Opcode {
		t.Fatalf("Opcode: got %d, want %d", got.Opcode, want.Opcode)
	}
	if got.Namespace != want.Namespace {
		t.Fatalf("Namespace: got %d, want %d", got.Namespace, want.Namespace)
	}
	if got.Hash != want.Hash {
		t.Fatal("Hash mismatch")
	}
}

func tcpPipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var srvConn net.Conn
	accepted := make(chan struct{})
	go func() {
		srvConn, _ = ln.Accept()
		close(accepted)
	}()

	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	<-accepted
	ln.Close()

	if srvConn == nil {
		cli.Close()
		t.Fatal("server accept failed")
	}
	return srvConn, cli
}

// Verify that ReadRequest properly handles data arriving in tiny chunks.
func TestReadRequest_SlowReader(t *testing.T) {
	value := []byte("slow-data")
	req := &Request{
		Opcode:    OpcodeObjectPut,
		Namespace: NSChunk,
		Hash:      sha256Hash("slow"),
		Value:     value,
	}

	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}

	// Wrap in a reader that returns 1 byte at a time.
	sr := &slowReader{data: buf.Bytes()}
	got, err := ReadRequest(sr)
	if err != nil {
		t.Fatalf("ReadRequest from slow reader: %v", err)
	}
	assertRequestEqual(t, req, got)
	if !bytes.Equal(got.Value, value) {
		t.Fatal("value mismatch")
	}
	got.Release()
}

type slowReader struct {
	data []byte
	off  int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.off]
	r.off++
	return 1, nil
}
