package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// OpenRanged opens an object as an io.ReadSeekCloser: seeks translate
// into ranged GETs, so http.ServeContent's Range handling works against
// bucket-backed content without buffering. The object's size is fixed at
// open time (one HEAD); returns ErrNotFound for missing objects.
func (c *Client) OpenRanged(ctx context.Context, key string) (io.ReadSeekCloser, error) {
	size, err := c.Head(ctx, key)
	if err != nil {
		return nil, err
	}
	return &rangedReader{ctx: ctx, client: c, key: key, size: size}, nil
}

// rangedReader is the lazy reader behind OpenRanged.
type rangedReader struct {
	ctx    context.Context
	client *Client
	key    string
	size   int64
	pos    int64
	cur    io.ReadCloser
}

func (r *rangedReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.cur == nil {
		rc, err := r.client.Get(r.ctx, r.key, r.pos)
		if err != nil {
			return 0, err
		}
		// The limit pins the size reported at open time: if the object is
		// replaced with longer content mid-read, the reader still ends at
		// the promised size instead of tearing the stream.
		r.cur = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(rc, r.size-r.pos), rc}
	}
	n, err := r.cur.Read(p)
	r.pos += int64(n)
	if errors.Is(err, io.EOF) && r.pos < r.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *rangedReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.pos + offset
	case io.SeekEnd:
		next = r.size + offset
	default:
		return 0, fmt.Errorf("s3client: invalid seek whence %d", whence)
	}
	if next < 0 {
		return 0, fmt.Errorf("s3client: negative seek position %d", next)
	}
	if next != r.pos && r.cur != nil {
		_ = r.cur.Close()
		r.cur = nil
	}
	r.pos = next
	return next, nil
}

func (r *rangedReader) Close() error {
	if r.cur != nil {
		return r.cur.Close()
	}
	return nil
}
