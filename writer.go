package veloxmiddleware

import (
	"net/http"
)

// responseWriter wraps http.ResponseWriter to capture PHP worker responses.
// This allows the middleware to inspect headers and body before deciding
// whether to intercept the response or pass it through to the client.
type responseWriter struct {
	code      int                 // HTTP status code
	data      []byte              // Response body from PHP worker
	hdrToSend map[string][]string // Response headers from PHP worker
}

// WriteHeader captures the status code from PHP worker response.
func (w *responseWriter) WriteHeader(code int) {
	w.code = code
}

// Write captures the response body from PHP worker.
func (w *responseWriter) Write(b []byte) (int, error) {
	w.data = append(w.data, b...)
	return len(b), nil
}

// Header returns the response headers that will be sent to the client.
// This allows the PHP worker to set headers that the middleware can inspect.
func (w *responseWriter) Header() http.Header {
	return w.hdrToSend
}
