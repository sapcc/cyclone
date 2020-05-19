/*******************************************************************************
*
* Copyright 2018 SAP SE
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You should have received a copy of the License along with this
* program. If not, you may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
*******************************************************************************/

package util

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/sapcc/go-bits/logg"
)

const (
	maxTotalRetryCount = 10
	maxRetryCount      = 3
)

//EnhancedGet is like http.Client.Get(), but recognizes if the HTTP server
//understands range requests, and downloads the file in segments in that case.
func EnhancedGet(client *http.Client, uri string, requestHeaders http.Header, segmentBytes uint64) (*http.Response, error) {
	d := downloader{
		Client:         client,
		URI:            uri,
		RequestHeaders: requestHeaders,
		SegmentBytes:   int64(segmentBytes),
		BytesTotal:     -1,
	}

	//make initial HTTP request that detects whether the source server supports
	//range requests
	resp, headers, err := d.getNextChunk()
	//if we receive a 416 (Requested Range Not Satisfiable), the most likely cause
	//is that the object is 0 bytes long, so even byte index 0 is already over
	//EOF -> fall back to a plain HTTP request in this case
	if resp != nil && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		logg.Info("received status code 416 -> falling back to plain GET for %s", uri)
		resp.Body.Close()
		req, err := d.buildRequest()
		if err != nil {
			return nil, err
		}
		return d.Client.Do(req)
	}

	//we're done here unless we receive a 206 (Partial Content) response; possible causes:
	//  * resp.StatusCode == 200 (server does not understand Range header)
	//  * resp.StatusCode == 304 (target is already up-to-date)
	//  * resp.StatusCode > 400 (other error)
	if err != nil || resp.StatusCode != http.StatusPartialContent {
		return resp, err
	}

	//actual response is 206, but we simulate the behavior of a 200 response
	resp.Status = "200 OK"
	resp.StatusCode = 200

	//if the current chunk contains the entire file, we do not need to hijack the body
	if d.BytesTotal > 0 && headers.ContentRangeLength == d.BytesTotal {
		return resp, err
	}

	//return the original response, but:
	//1. hijack the response body so that the next segments will be loaded once
	//the current response body is exhausted (the `struct downloader` is wrapped
	//into a util.FullReader because ncw/swift seems to get confused when Read()
	//does not fill the provided buffer completely)
	d.Reader = resp.Body
	resp.Body = &FullReader{&d}
	//2. report the total file size in the response, so that the caller can
	//decide whether to PUT this directly or as a large object
	if d.BytesTotal > 0 {
		resp.ContentLength = d.BytesTotal
	}
	return resp, err
}

//downloader is an io.ReadCloser that downloads a file from a Source in
//segments by using the HTTP request parameter "Range" [RFC 7233].
//
//A downloader is usually constructed by DownloadFile() which probes the server
//for Range support and otherwise falls back on downloading the entire file at
//once.
type downloader struct {
	//the original arguments to EnhancedGet()
	Client         *http.Client
	URI            string
	RequestHeaders http.Header
	SegmentBytes   int64
	//this object's internal state
	Etag       string        //we track the source URL's Etag to detect changes mid-transfer
	BytesRead  int64         //how many bytes have already been read out of this Reader
	BytesTotal int64         //the total Content-Length the file, or -1 if not known
	Reader     io.ReadCloser //current, not-yet-exhausted, response.Body, or nil after EOF or error
	Err        error         //non-nil after error
	//retry handling
	LastErrorAtBytesRead int64 //at which offset the last read error occurred
	LastErrorRetryCount  int   //retry counter for said offset, or 0 if no error occurred before
	TotalRetryCount      int   //retry counter for all read errors encountered at any offset
}

type parsedResponseHeaders struct {
	ContentRangeStart  int64
	ContentRangeLength int64
}

func (d *downloader) buildRequest() (*http.Request, error) {
	req, err := http.NewRequest("GET", d.URI, nil)
	if err != nil {
		return nil, err
	}
	for key, val := range d.RequestHeaders {
		req.Header[key] = val
	}
	return req, nil
}

func (d *downloader) getNextChunk() (*http.Response, *parsedResponseHeaders, error) {
	req, err := d.buildRequest()
	if err != nil {
		return nil, nil, err
	}

	//add Range header
	start := d.BytesRead
	end := start + d.SegmentBytes
	if d.BytesTotal > 0 && end > d.BytesTotal {
		end = d.BytesTotal
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end-1)) //end index is inclusive!

	//execute request
	resp, err := d.Client.Do(req)
	if err != nil {
		return resp, nil, err
	}

	//check Etag
	etag := resp.Header.Get("Etag")
	if d.Etag == "" {
		d.Etag = etag
	} else if d.Etag != etag {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("Etag has changed mid-transfer: %q -> %q", d.Etag, etag)
	}

	//parse Content-Range header
	var headers parsedResponseHeaders
	if resp.StatusCode == 206 {
		contentRange := resp.Header.Get("Content-Range")
		start, stop, total, ok := parseContentRange(contentRange)
		if !ok {
			resp.Body.Close()
			return nil, nil, errors.New("malformed response header: Content-Range: " + contentRange)
		}
		headers.ContentRangeStart = start
		headers.ContentRangeLength = stop - start + 1 //stop index is inclusive!
		if d.BytesTotal == -1 {
			d.BytesTotal = total
		} else if d.BytesTotal != total {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("Content-Length has changed mid-transfer: %d -> %d", d.BytesTotal, total)
		}
	}

	return resp, &headers, nil
}

//Matches values of Content-Range response header [RFC7233, section 4.2] for
//successful range responses (i.e. HTTP status code 206, not 416).
var contentRangeRx = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/(\*|[0-9]+)$`)

func parseContentRange(headerValue string) (start, stop, total int64, ok bool) {
	match := contentRangeRx.FindStringSubmatch(headerValue)
	if match == nil {
		return 0, 0, 0, false
	}

	var err error
	start, err = strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	stop, err = strconv.ParseInt(match[2], 10, 64)
	if err != nil || stop < start {
		return 0, 0, 0, false
	}
	total = -1
	if match[3] != "*" {
		total, err = strconv.ParseInt(match[3], 10, 64)
		if err != nil {
			return 0, 0, 0, false
		}
	}
	return start, stop, total, true
}

//Read implements the io.ReadCloser interface.
func (d *downloader) Read(buf []byte) (int, error) {
	//if we don't have a response body, we're at EOF
	if d.Reader == nil {
		return 0, io.EOF
	}

	//read from the current response body
	bytesRead, err := d.Reader.Read(buf)
	d.BytesRead += int64(bytesRead)
	switch err {
	case nil:
		return bytesRead, err
	case io.EOF:
		//current response body is EOF -> close it
		err = d.Reader.Close()
		d.Reader = nil
		if err != nil {
			return bytesRead, err
		}
		d.Reader = nil
	default:
		//unexpected read error
		if !d.shouldRetry() {
			return bytesRead, err
		}
		logg.Error("restarting GET %s after read error at offset %d: %s",
			d.URI, d.BytesRead, err.Error(),
		)
		err := d.Reader.Close()
		if err != nil {
			logg.Error(
				"encountered additional error when trying to close the existing reader: %s",
				err.Error(),
			)
		}
	}

	//is there a next chunk?
	if d.BytesRead == d.BytesTotal {
		return bytesRead, io.EOF
	}

	//get next chunk
	resp, headers, err := d.getNextChunk()
	if err != nil {
		return bytesRead, err
	}
	if headers.ContentRangeStart != d.BytesRead {
		resp.Body.Close()
		return bytesRead, fmt.Errorf(
			"expected next segment to start at offset %d, but starts at %d",
			d.BytesRead, headers.ContentRangeStart,
		)
	}
	d.Reader = resp.Body
	return bytesRead, nil
}

//Close implements the io.ReadCloser interface.
func (d *downloader) Close() error {
	if d.Reader != nil {
		return d.Reader.Close()
	}
	return nil
}

//This function is called when a read error is encountered, and decides whether
//to retry or not.
func (d *downloader) shouldRetry() bool {
	//never restart transfer of the same file more than 10 times
	d.TotalRetryCount++
	if d.TotalRetryCount > maxTotalRetryCount {
		logg.Info("giving up on GET %s after %d read errors", d.URI, maxTotalRetryCount)
		return false
	}

	//if there was no error at this offset before, always retry
	if d.LastErrorAtBytesRead != d.BytesRead {
		d.LastErrorAtBytesRead = d.BytesRead
		d.LastErrorRetryCount = 0
		return true
	}

	//only retry an error at the same offset for 3 times
	d.LastErrorRetryCount++
	if d.LastErrorRetryCount > maxRetryCount {
		logg.Info("giving up on GET %s after %d read errors at the same offset (%d)",
			d.URI,
			maxRetryCount,
			d.LastErrorAtBytesRead,
		)
		return false
	}
	return true
}
