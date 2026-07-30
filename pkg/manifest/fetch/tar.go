package fetch

import (
	"fmt"
	"os"
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"golang.org/x/sys/unix"
)

type storedRangeSource interface {
	StoredRangeAt(offset, limit uint64) (fileOffset, length uint64, ok bool, err error)
}

// tarFileStream is a local tarstream artifact as a Stream: the
// platform's at-rest container for images, snapshot bundles and
// overlays (one sparse payload plus an empty digest marker). The envelope's hole map drives
// RunAt; ReadAt is offset arithmetic over the packed region via
// *os.File.ReadAt (pread), meeting Stream's concurrent random-access
// contract. Nothing is unpacked.
type tarFileStream struct {
	sparse.Source
	f       *os.File
	digest  string
	mapOnce sync.Once
	mapped  []byte
	mapErr  error
}

// OpenTarStream opens the artifact at path (a tarstream envelope whose first
// entry is the payload and whose final empty entry declares SHA256) and returns
// it as a Stream that also implements tarstream.Digester. The caller owns the
// returned stream and must Close it. A non-tarstream file or a tar without the
// marker fails loudly.
func OpenTarStream(path string) (Stream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fetch: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("fetch: stat %s: %w", path, err)
	}
	src, _, err := tarstream.SourceAt(f, st.Size(), "")
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("fetch: %s: not a tarstream artifact: %w", path, err)
	}
	d, ok := src.(tarstream.Digester)
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("fetch: %s: tarstream artifact missing digest marker", path)
	}
	return &tarFileStream{Source: src, f: f, digest: d.Digest()}, nil
}

func (s *tarFileStream) Digest() string { return s.digest }

// BorrowAt returns a read-only mmap view of a packed Data extent. Mapping is
// lazy so ordinary Stream users retain the existing pread-only behaviour.
func (s *tarFileStream) BorrowAt(offset, limit uint64) ([]byte, bool, error) {
	locator, ok := s.Source.(storedRangeSource)
	if !ok {
		return nil, false, nil
	}
	fileOffset, length, ok, err := locator.StoredRangeAt(offset, limit)
	if err != nil || !ok {
		return nil, ok, err
	}

	s.mapOnce.Do(func() {
		info, err := s.f.Stat()
		if err != nil {
			s.mapErr = err
			return
		}
		if info.Size() <= 0 || uint64(info.Size()) > uint64(^uint(0)>>1) {
			s.mapErr = fmt.Errorf("fetch: cannot mmap tarstream size %d", info.Size())
			return
		}
		s.mapped, s.mapErr = unix.Mmap(
			int(s.f.Fd()),
			0,
			int(info.Size()),
			unix.PROT_READ,
			unix.MAP_PRIVATE,
		)
	})
	if s.mapErr != nil {
		return nil, false, s.mapErr
	}
	end := fileOffset + length
	if end < fileOffset || end > uint64(len(s.mapped)) {
		return nil, false, fmt.Errorf(
			"fetch: borrowed tarstream range [%d,%d) exceeds mapped size %d",
			fileOffset,
			end,
			len(s.mapped),
		)
	}
	return s.mapped[fileOffset:end], true, nil
}

func (s *tarFileStream) Close() error {
	var unmapErr error
	if s.mapped != nil {
		unmapErr = unix.Munmap(s.mapped)
		s.mapped = nil
	}
	closeErr := s.f.Close()
	if unmapErr != nil {
		return unmapErr
	}
	return closeErr
}

var _ BorrowingStream = (*tarFileStream)(nil)
