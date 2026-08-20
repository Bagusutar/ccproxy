package main

import (
	"bytes"
	"io"
)

// prependReadCloser replays a consumed prefix while preserving ownership of the
// original response body. Closing the wrapper always closes the network body.
type prependReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *prependReadCloser) Close() error { return r.closer.Close() }

func prependBody(prefix []byte, body io.ReadCloser) io.ReadCloser {
	return &prependReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), closer: body}
}
