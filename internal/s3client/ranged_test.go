package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
)

// rangedFixture serves one mutable object with proper Range handling.
type rangedFixture struct {
	mu      sync.Mutex
	content []byte
}

func (f *rangedFixture) set(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content = b
}

func (f *rangedFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	content := f.content
	f.mu.Unlock()
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(len(content)))
		return
	}
	var offset int64
	if rng := r.Header.Get("Range"); rng != "" {
		if _, err := fmt.Sscanf(rng, "bytes=%d-", &offset); err != nil || offset >= int64(len(content)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(content)-1, len(content)))
		w.WriteHeader(http.StatusPartialContent)
	}
	w.Write(content[offset:])
}

func TestOpenRangedMissing(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	if _, err := c.OpenRanged(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("OpenRanged(missing) = %v, want ErrNotFound", err)
	}
}

func TestRangedReaderWindow(t *testing.T) {
	t.Parallel()
	content := []byte("0123456789abcdefghij")
	fx := &rangedFixture{content: content}
	c, _ := newTestClient(t, fx)
	r, err := c.OpenRanged(context.Background(), "obj")
	if err != nil {
		t.Fatalf("OpenRanged: %v", err)
	}
	defer r.Close()

	// SeekEnd reports the size, and reading there is EOF without any GET.
	if end, err := r.Seek(0, io.SeekEnd); err != nil || end != int64(len(content)) {
		t.Fatalf("Seek(0, End) = %d, %v", end, err)
	}
	buf := make([]byte, 5)
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at end = %d, %v, want 0, io.EOF", n, err)
	}
	// A window read after seeking back issues a fresh ranged GET.
	if _, err := r.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "abcde" {
		t.Fatalf("window read = %q, %v", buf, err)
	}
	// SeekCurrent continues from the position.
	if pos, err := r.Seek(2, io.SeekCurrent); err != nil || pos != 17 {
		t.Fatalf("Seek(2, Current) = %d, %v", pos, err)
	}
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != "hij" {
		t.Fatalf("tail read = %q, %v", rest, err)
	}
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read at EOF = %d, %v", n, err)
	}
	// SeekEnd with a negative offset addresses from the tail.
	if pos, err := r.Seek(-3, io.SeekEnd); err != nil || pos != 17 {
		t.Fatalf("Seek(-3, End) = %d, %v", pos, err)
	}
	if tail, err := io.ReadAll(r); err != nil || string(tail) != "hij" {
		t.Fatalf("tail via SeekEnd = %q, %v", tail, err)
	}
	// Seeking to exactly 0 is legal; negative and bogus seeks fail.
	if pos, err := r.Seek(0, io.SeekStart); err != nil || pos != 0 {
		t.Fatalf("Seek(0, Start) = %d, %v", pos, err)
	}
	if _, err := io.ReadFull(r, buf[:1]); err != nil || buf[0] != '0' {
		t.Fatalf("read after rewind = %q, %v", buf[:1], err)
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Error("negative seek accepted")
	}
	if _, err := r.Seek(0, 99); err == nil {
		t.Error("bogus whence accepted")
	}

	// Closing a reader that never opened a stream is a no-op.
	unread, err := c.OpenRanged(context.Background(), "obj")
	if err != nil {
		t.Fatal(err)
	}
	if err := unread.Close(); err != nil {
		t.Errorf("Close without read: %v", err)
	}
}

// TestRangedReaderHonorsOpenTimeSize pins the size contract: the reader
// serves exactly the size it reported at open time, even if the object
// grows underneath it (http.ServeContent trusts this for Content-Length).
func TestRangedReaderHonorsOpenTimeSize(t *testing.T) {
	t.Parallel()
	fx := &rangedFixture{content: []byte("0123456789")}
	c, _ := newTestClient(t, fx)
	r, err := c.OpenRanged(context.Background(), "obj")
	if err != nil {
		t.Fatalf("OpenRanged: %v", err)
	}
	defer r.Close()
	fx.set([]byte("0123456789EXTRA"))
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "456789" {
		t.Errorf("ReadAll from pos 4 = %q, %v, want \"456789\"", got, err)
	}
}

// TestRangedReaderTruncationIsAnError pins the opposite failure: an
// object that shrinks mid-read surfaces io.ErrUnexpectedEOF, not a silent
// short stream.
func TestRangedReaderTruncationIsAnError(t *testing.T) {
	t.Parallel()
	fx := &rangedFixture{content: []byte("0123456789")}
	c, _ := newTestClient(t, fx)
	r, err := c.OpenRanged(context.Background(), "obj")
	if err != nil {
		t.Fatalf("OpenRanged: %v", err)
	}
	defer r.Close()
	fx.set([]byte("0123"))
	_, err = io.ReadAll(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadAll over truncated object = %v, want io.ErrUnexpectedEOF", err)
	}
}
