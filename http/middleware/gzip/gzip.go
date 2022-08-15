package gzip

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Config defines the config for Gzip middleware.
type Config struct {
	// Skipper defines a function to skip middleware.
	Skipper middleware.Skipper

	// Gzip compression level.
	// Optional. Default value -1.
	Level int

	// Length threshold before gzip compression
	// is used. Optional. Default value 0
	MinLength int

	// Content-Types to compress. Empty for all
	// files. Optional. Default value "text/plain" and "text/html"
	ContentTypes []string
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

const gzipScheme = "gzip"

const (
	BestCompression    = gzip.BestCompression
	BestSpeed          = gzip.BestSpeed
	DefaultCompression = gzip.DefaultCompression
	NoCompression      = gzip.NoCompression
)

// DefaultConfig is the default Gzip middleware config.
var DefaultConfig = Config{
	Skipper:      middleware.DefaultSkipper,
	Level:        -1,
	MinLength:    0,
	ContentTypes: []string{"text/plain", "text/html"},
}

// New returns a middleware which compresses HTTP response using gzip compression
// scheme.
func New() echo.MiddlewareFunc {
	return NewWithConfig(DefaultConfig)
}

// NewWithConfig return Gzip middleware with config.
// See: `New()`.
func NewWithConfig(config Config) echo.MiddlewareFunc {
	// Defaults
	if config.Skipper == nil {
		config.Skipper = DefaultConfig.Skipper
	}

	if config.Level == 0 {
		config.Level = DefaultConfig.Level
	}

	if config.MinLength < 0 {
		config.MinLength = DefaultConfig.MinLength
	}

	if config.ContentTypes == nil {
		config.ContentTypes = DefaultConfig.ContentTypes
	}

	pool := gzipPool(config)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}

			res := c.Response()
			res.Header().Add(echo.HeaderVary, echo.HeaderAcceptEncoding)
			if shouldCompress(c, config.ContentTypes) {
				res.Header().Set(echo.HeaderContentEncoding, gzipScheme) // Issue #806
				i := pool.Get()
				w, ok := i.(*gzip.Writer)
				if !ok {
					return echo.NewHTTPError(http.StatusInternalServerError, i.(error).Error())
				}
				rw := res.Writer
				w.Reset(rw)
				defer func() {
					if res.Size == 0 {
						if res.Header().Get(echo.HeaderContentEncoding) == gzipScheme {
							res.Header().Del(echo.HeaderContentEncoding)
						}
						// We have to reset response to it's pristine state when
						// nothing is written to body or error is returned.
						// See issue #424, #407.
						res.Writer = rw
						w.Reset(io.Discard)
					}
					w.Close()
					pool.Put(w)
				}()
				grw := &gzipResponseWriter{Writer: w, ResponseWriter: rw}
				res.Writer = grw
			}
			return next(c)
		}
	}
}

func shouldCompress(c echo.Context, contentTypes []string) bool {
	if !strings.Contains(c.Request().Header.Get(echo.HeaderAcceptEncoding), gzipScheme) ||
		strings.Contains(c.Request().Header.Get("Connection"), "Upgrade") ||
		strings.Contains(c.Request().Header.Get(echo.HeaderContentType), "text/event-stream") {

		return false
	}

	// If no allowed content types are given, compress all
	if len(contentTypes) == 0 {
		return true
	}

	// Iterate through the allowed content types and return true if the content type matches
	responseContentType := c.Response().Header().Get(echo.HeaderContentType)

	for _, contentType := range contentTypes {
		if strings.Contains(responseContentType, contentType) {
			return true
		}
	}

	return false
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if code == http.StatusNoContent { // Issue #489
		w.ResponseWriter.Header().Del(echo.HeaderContentEncoding)
	}
	w.Header().Del(echo.HeaderContentLength) // Issue #444
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if w.Header().Get(echo.HeaderContentType) == "" {
		w.Header().Set(echo.HeaderContentType, http.DetectContentType(b))
	}

	return w.Writer.Write(b)
}

func (w *gzipResponseWriter) Flush() {
	w.Writer.(*gzip.Writer).Flush()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

func (w *gzipResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func gzipPool(config Config) sync.Pool {
	return sync.Pool{
		New: func() interface{} {
			w, err := gzip.NewWriterLevel(io.Discard, config.Level)
			if err != nil {
				return err
			}
			return w
		},
	}
}
