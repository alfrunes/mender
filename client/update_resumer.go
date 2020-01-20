// Copyright 2017 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package client

import (
	"fmt"
	"github.com/mendersoftware/log"
	"github.com/pkg/errors"
	"io"
	"io/ioutil"
	"time"
)

// Normally one minute, but used in tests to lower the interval to avoid
// waiting.
var ExponentialBackoffSmallestUnit time.Duration = time.Minute

// ResumeFunc contains a function that will resume the update and returns
// a new ReadCloser as well as the new offset and content length on success,
// otherwise an appropriate error will be returned.
type ResumeFunc func(offset int64) (io.ReadCloser, int64, int64, error)

type UpdateResumer struct {
	stream        io.ReadCloser
	offset        int64
	contentLength int64
	retryAttempts int
	maxWait       time.Duration
	resumeFunc    ResumeFunc
}

// Note: It is important that nothing has been read from the stream yet.
func NewUpdateResumer(stream io.ReadCloser,
	contentLength int64,
	maxWait time.Duration,
	resumeFunc ResumeFunc) *UpdateResumer {

	return &UpdateResumer{
		stream:        stream,
		contentLength: contentLength,
		maxWait:       maxWait,
		resumeFunc:    resumeFunc,
	}
}

func (h *UpdateResumer) Read(buf []byte) (int, error) {
	origOffset := h.offset
	for {
		bytesRead, err := h.stream.Read(buf[h.offset-origOffset:])
		if bytesRead > 0 {
			h.offset += int64(bytesRead)
		}
		if err == nil ||
			h.offset <= 0 ||
			(err == io.EOF && h.offset >= h.contentLength) {

			return int(h.offset - origOffset), err
		}

		// If we get here we have unexpected EOF, either an actual unexpected
		// EOF, or a normal EOF, but with an unexpected number of bytes. This is
		// a sign that we should try to resume from the same position.
		for {
			log.Errorf("Download connection broken: %s", err.Error())
			waitTime, err := GetExponentialBackoffTime(h.retryAttempts, h.maxWait)
			if err != nil {
				return int(h.offset - origOffset),
					errors.Wrapf(err, "Cannot resume download")
			}

			log.Infof("Resuming download in %s", waitTime.String())
			h.retryAttempts += 1

			time.Sleep(waitTime)

			log.Infof("Attempting to resume artifact download "+
				"from offset %d", h.offset)

			body, offset, length, err := h.resumeFunc(h.offset)
			if err != nil {
				log.Infof("Download resume request failed: %s", err.Error())
				continue
			}

			if offset > h.offset {
				return -1, fmt.Errorf(
					"HTTP server did not return expected "+
						"range. Expected %d, got %d",
					h.offset, offset)
			} else if offset < h.offset {
				bytesRead, err := io.CopyN(
					ioutil.Discard, body, h.offset-offset)
				if err == io.ErrUnexpectedEOF {
					// Treat this specifically to force a
					// retry in the outer function.
					return -1, err
				} else if err != nil ||
					bytesRead != h.offset-offset {
					return -1, errors.Wrapf(err,
						"Could not resume download, "+
							"unable to catch up to "+
							"offset %d from offset %d",
						h.offset, offset)
				}
			}
			if length != h.contentLength {
				return -1, fmt.Errorf(
					"HTTP server returned inconsistent "+
						"range header; expected: "+
						"%d != received: %d",
					h.contentLength, length)
			}

			h.stream.Close()
			h.stream = body
			break
		}
		// Repeat from the top.
	}
}

func (h *UpdateResumer) Close() error {
	return h.stream.Close()
}

// Simple algorithm: Start with one minute, and try three times, then double
// interval (maxInterval is maximum) and try again. Repeat until we tried
// three times with maxInterval.
func GetExponentialBackoffTime(tried int, maxInterval time.Duration) (time.Duration, error) {
	const perIntervalAttempts = 3

	interval := 1 * ExponentialBackoffSmallestUnit
	nextInterval := interval

	for c := 0; c <= tried; c += perIntervalAttempts {
		interval = nextInterval
		nextInterval *= 2
		if interval >= maxInterval {
			if tried-c >= perIntervalAttempts {
				// At max interval and already tried three
				// times. Give up.
				return 0, errors.New("Tried maximum amount of times")
			}

			// Don't use less than the smallest unit, usually one
			// minute.
			if maxInterval < ExponentialBackoffSmallestUnit {
				return ExponentialBackoffSmallestUnit, nil
			}
			return maxInterval, nil
		}
	}

	return interval, nil
}
