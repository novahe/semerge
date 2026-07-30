package fetch

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// TestOpenTarStream: a tarstream artifact serves as a Stream — hole
// map from the envelope, concurrent random reads, nothing unpacked.
func TestOpenTarStream(t *testing.T) {
	const size = 2 << 20
	logical := make([]byte, size)
	copy(logical, "HEAD")
	copy(logical[1<<20:], "TAIL")
	holes := []sparse.Extent{{Offset: 4096, Size: (1 << 20) - 4096}, {Offset: (1 << 20) + 4096, Size: (1 << 20) - 4096}}

	src, err := sparse.NewSource(bytes.NewReader(logical), size, holes)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "img.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, err := tarstream.WriteTo(context.Background(), f, "image", src)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	st, err := OpenTarStream(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d, ok := st.(tarstream.Digester)
	if !ok {
		t.Fatal("OpenTarStream result does not implement tarstream.Digester")
	}
	if d.Digest() != wantDigest {
		t.Fatalf("digest = %q; want %q", d.Digest(), wantDigest)
	}
	if st.Size() != size {
		t.Fatalf("size = %d", st.Size())
	}
	if kind, end, err := st.RunAt(4096, size); kind != sparse.Hole || end != 1<<20 || err != nil {
		t.Fatalf("RunAt(4096) = (%v, %d, %v)", kind, end, err)
	}
	borrower, ok := st.(BorrowingStream)
	if !ok {
		t.Fatal("local tar stream does not expose BorrowAt")
	}
	borrowed, ok, err := borrower.BorrowAt(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(borrowed) != 4096 {
		t.Fatalf("BorrowAt(data) = len %d, ok %v", len(borrowed), ok)
	}
	if !bytes.Equal(borrowed, logical[:4096]) {
		t.Fatal("borrowed data does not match logical content")
	}
	if borrowed, ok, err := borrower.BorrowAt(4096, 4096); err != nil || ok || borrowed != nil {
		t.Fatalf("BorrowAt(hole) = len %d, ok %v, err %v", len(borrowed), ok, err)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(off uint64) {
			defer wg.Done()
			buf := make([]byte, 4)
			if _, err := st.ReadAt(ctx, buf, off); err != nil && err != io.EOF {
				errs <- err
				return
			}
			want := logical[off : off+4]
			if !bytes.Equal(buf, want) {
				errs <- io.ErrUnexpectedEOF
			}
		}(uint64(i%2) << 20)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Non-tar input fails loudly.
	raw := filepath.Join(t.TempDir(), "raw.bin")
	os.WriteFile(raw, []byte("not a tar at all"), 0o644)
	if _, err := OpenTarStream(raw); err == nil {
		t.Fatal("raw file accepted as tar artifact")
	}

	// A generic tar remains readable through tarstream.SourceAt, but platform
	// artifact consumers require the digest capability.
	plain := filepath.Join(t.TempDir(), "plain.tar")
	pf, err := os.Create(plain)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(pf)
	if err := tw.WriteHeader(&tar.Header{Name: "image", Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pf.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTarStream(plain); err == nil {
		t.Fatal("markerless tar accepted as platform artifact")
	}
}
