/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package docker

import (
	"bytes"
	"fmt"
	"io"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

const maxRetry = 3

type httpReadSeeker struct {
	size   int64
	offset int64
	rc     io.ReadCloser
	open   func(offset int64) (io.ReadCloser, error)
	closed bool

	errsWithNoProgress int
}

func newHTTPReadSeeker(size int64, open func(offset int64) (io.ReadCloser, error)) (io.ReadCloser, error) {
	return &httpReadSeeker{
		size: size,
		open: open,
	}, nil
}

func (hrs *httpReadSeeker) Read(p []byte) (n int, err error) {
	if hrs.closed {
		return 0, io.EOF
	}

	rd, err := hrs.reader()
	if err != nil {
		return 0, err
	}

	n, err = rd.Read(p)
	hrs.offset += int64(n)
	if n > 0 || err == nil {
		hrs.errsWithNoProgress = 0
	}
	switch err {
	case io.ErrUnexpectedEOF:
		// connection closed unexpectedly. try reconnecting.
		if n == 0 {
			hrs.errsWithNoProgress++
			if hrs.errsWithNoProgress > maxRetry {
				return // too many retries for this offset with no progress
			}
		}
		if hrs.rc != nil {
			if clsErr := hrs.rc.Close(); clsErr != nil {
				log.L.WithError(clsErr).Error("httpReadSeeker: failed to close ReadCloser")
			}
			hrs.rc = nil
		}
		if _, err2 := hrs.reader(); err2 == nil {
			return n, nil
		}
	case io.EOF:
		// The CRI's imagePullProgressTimeout relies on responseBody.Close to
		// update the process monitor's status. If the err is io.EOF, close
		// the connection since there is no more available data.
		if hrs.rc != nil {
			if clsErr := hrs.rc.Close(); clsErr != nil {
				log.L.WithError(clsErr).Error("httpReadSeeker: failed to close ReadCloser after io.EOF")
			}
			hrs.rc = nil
		}
	}
	return
}

func (hrs *httpReadSeeker) Close() error {
	if hrs.closed {
		return nil
	}
	hrs.closed = true
	if hrs.rc != nil {
		return hrs.rc.Close()
	}

	return nil
}

func (hrs *httpReadSeeker) ReadAt(p []byte, offset int64) (n int, err error) {
	if hrs.closed {
		return 0, fmt.Errorf("httpReadSeeker.ReadAt: closed: %w", errdefs.ErrUnavailable)
	}

	if offset < 0 {
		return 0, fmt.Errorf("httpReadSeeker.ReadAt: negative offset: %w", errdefs.ErrInvalidArgument)
	}

	if hrs.size != -1 && offset >= hrs.size {
		return 0, io.EOF
	}

	if hrs.open == nil {
		return 0, fmt.Errorf("httpReadSeeker.ReadAt: cannot open: %w", errdefs.ErrNotImplemented)
	}

	rc, err := hrs.open(offset)
	if err != nil {
		return 0, fmt.Errorf("httpReadSeeker.ReadAt: failed to open at offset %d: %w", offset, err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			log.L.WithError(closeErr).Error("httpReadSeeker.ReadAt: failed to close ReadCloser")
		}
	}()

	return io.ReadFull(rc, p)
}

func (hrs *httpReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if hrs.closed {
		return 0, fmt.Errorf("Fetcher.Seek: closed: %w", errdefs.ErrUnavailable)
	}

	abs := hrs.offset
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs += offset
	case io.SeekEnd:
		if hrs.size == -1 {
			return 0, fmt.Errorf("Fetcher.Seek: unknown size, cannot seek from end: %w", errdefs.ErrUnavailable)
		}
		abs = hrs.size + offset
	default:
		return 0, fmt.Errorf("Fetcher.Seek: invalid whence: %w", errdefs.ErrInvalidArgument)
	}

	if abs < 0 {
		return 0, fmt.Errorf("Fetcher.Seek: negative offset: %w", errdefs.ErrInvalidArgument)
	}

	if abs != hrs.offset {
		if hrs.rc != nil {
			if err := hrs.rc.Close(); err != nil {
				log.L.WithError(err).Error("Fetcher.Seek: failed to close ReadCloser")
			}

			hrs.rc = nil
		}

		hrs.offset = abs
	}

	return hrs.offset, nil
}

func (hrs *httpReadSeeker) reader() (io.Reader, error) {
	if hrs.rc != nil {
		return hrs.rc, nil
	}

	if hrs.size == -1 || hrs.offset < hrs.size {
		// only try to reopen the body request if we are seeking to a value
		// less than the actual size.
		if hrs.open == nil {
			return nil, fmt.Errorf("cannot open: %w", errdefs.ErrNotImplemented)
		}

		rc, err := hrs.open(hrs.offset)
		if err != nil {
			return nil, fmt.Errorf("httpReadSeeker: failed open: %w", err)
		}

		if hrs.rc != nil {
			if err := hrs.rc.Close(); err != nil {
				log.L.WithError(err).Error("httpReadSeeker: failed to close ReadCloser")
			}
		}
		hrs.rc = rc
	} else {
		// There is an edge case here where offset == size of the content. If
		// we seek, we will probably get an error for content that cannot be
		// sought (?). In that case, we should err on committing the content,
		// as the length is already satisfied but we just return the empty
		// reader instead.

		hrs.rc = io.NopCloser(bytes.NewReader([]byte{}))
	}

	return hrs.rc, nil
}
