package api

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

var gzPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }}

type gzWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (w *gzWriter) Write(b []byte) (int, error) { return w.gz.Write(b) }

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *gzWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzWriter) WriteHeader(code int) {
	w.Header().Del("Content-Length")
	if code == http.StatusNotModified || code == http.StatusNoContent {
		w.Header().Del("Content-Encoding")
	}
	w.ResponseWriter.WriteHeader(code)
}

// gzipped compresses responses for clients that accept it (all city
// vehicles: ~500 KB of JSON, ~60 KB gzipped). Streams and metrics are left
// alone: SSE events must reach the client as they are written.
func gzipped(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.URL.Path == "/v1/stream" || r.URL.Path == "/metrics" {
			h.ServeHTTP(w, r)
			return
		}
		gz := gzPool.Get().(*gzip.Writer)
		defer gzPool.Put(gz)
		gz.Reset(w)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzWriter{ResponseWriter: w, gz: gz}
		h.ServeHTTP(gw, r)
		if w.Header().Get("Content-Encoding") == "gzip" {
			gz.Close()
		}
	})
}
