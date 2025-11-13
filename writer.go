package velox

import "net/http"

// writer is a custom ResponseWriter that captures the response
type writer struct {
	code      int
	data      []byte
	hdrToSend map[string][]string
}

// WriteHeader captures the HTTP status code
func (w *writer) WriteHeader(code int) {
	w.code = code
}

// Write captures the response body
func (w *writer) Write(b []byte) (int, error) {
	w.data = append(w.data, b...)
	return len(b), nil
}

// Header returns the response headers
func (w *writer) Header() http.Header {
	return w.hdrToSend
}
