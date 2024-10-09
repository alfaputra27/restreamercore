// Package session is a HLS session middleware for Gin
package session

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/url"
	urlpath "path"
	"path/filepath"
	"strings"

	"github.com/datarhei/core/v16/net"
	"github.com/lithammer/shortuuid/v4"

	"github.com/labstack/echo/v4"
)

func (h *handler) handleHLS(c echo.Context, ctxuser string, data map[string]interface{}, next echo.HandlerFunc) error {
	req := c.Request()

	if req.Method == "PUT" || req.Method == "POST" {
		return h.handleHLSIngress(c, ctxuser, data, next)
	} else if req.Method == "GET" || req.Method == "HEAD" {
		return h.handleHLSEgress(c, ctxuser, data, next)
	}

	return next(c)
}

func (h *handler) handleHLSIngress(c echo.Context, _ string, data map[string]interface{}, next echo.HandlerFunc) error {
	req := c.Request()
	path := req.URL.Path

	if strings.HasSuffix(path, ".m3u8") {
		// Read out the path of the .ts files and look them up in the ts-map.
		// Add it as ingress for the respective "sessionId". The "sessionId" is the .m3u8 file name.
		reader := req.Body
		r := &bodyReader{
			reader: req.Body,
		}
		req.Body = r

		defer func() {
			req.Body = reader

			if r.size == 0 {
				return
			}

			if !h.hlsIngressCollector.IsKnownSession(path) {
				ip, _ := net.AnonymizeIPString(c.RealIP())

				// Register a new session
				reference := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
				h.hlsIngressCollector.RegisterAndActivate(path, reference, path, ip)
				h.hlsIngressCollector.Extra(path, data)
			}

			h.hlsIngressCollector.Ingress(path, headerSize(req.Header))
			h.hlsIngressCollector.Ingress(path, r.size)

			segments := r.getSegments(urlpath.Dir(path))

			if len(segments) != 0 {
				h.lock.Lock()
				for _, seg := range segments {
					if size, ok := h.rxsegments[seg]; ok {
						// Update ingress value
						h.hlsIngressCollector.Ingress(path, size)
						delete(h.rxsegments, seg)
					}
				}
				h.lock.Unlock()
			}
		}()
	} else if strings.HasSuffix(path, ".ts") {
		// Get the size of the .ts file and store it in the ts-map for later use.
		reader := req.Body
		r := &bodysizeReader{
			reader: req.Body,
		}
		req.Body = r

		defer func() {
			req.Body = reader

			if r.size != 0 {
				h.lock.Lock()
				h.rxsegments[path] = r.size + headerSize(req.Header)
				h.lock.Unlock()
			}
		}()
	}

	return next(c)
}

func (h *handler) handleHLSEgress(c echo.Context, _ string, data map[string]interface{}, next echo.HandlerFunc) error {
	req := c.Request()
	res := c.Response()

	if !h.hlsEgressCollector.IsCollectableIP(c.RealIP()) {
		return next(c)
	}

	path := req.URL.Path
	sessionID := c.QueryParam("session")

	isM3U8 := strings.HasSuffix(path, ".m3u8")
	isTS := strings.HasSuffix(path, ".ts")

	rewrite := false

	if isM3U8 {
		if !h.hlsEgressCollector.IsKnownSession(sessionID) {
			if h.hlsEgressCollector.IsSessionsExceeded() {
				return echo.NewHTTPError(509, "Number of sessions exceeded")
			}

			streamBitrate := h.hlsIngressCollector.SessionTopIngressBitrate(path) * 2.0 // Multiply by 2 to cover the initial peak
			maxBitrate := h.hlsEgressCollector.MaxEgressBitrate()

			if maxBitrate > 0.0 {
				currentBitrate := h.hlsEgressCollector.CompanionTopEgressBitrate() * 1.15

				// Add the new session's top bitrate to the ingress top bitrate
				resultingBitrate := currentBitrate + streamBitrate

				if resultingBitrate >= maxBitrate {
					return echo.NewHTTPError(509, "Bitrate limit exceeded")
				}
			}

			if len(sessionID) != 0 {
				if !h.reSessionID.MatchString(sessionID) {
					return echo.NewHTTPError(http.StatusForbidden)
				}

				referrer := req.Header.Get("Referer")
				if u, err := url.Parse(referrer); err == nil {
					referrer = u.Host
				}

				reference := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))

				// Register a new session
				h.hlsEgressCollector.Register(sessionID, reference, path, referrer)
				h.hlsEgressCollector.Extra(sessionID, data)

				// Give the new session an initial top bitrate
				h.hlsEgressCollector.SessionSetTopEgressBitrate(sessionID, streamBitrate)
			}
		}

		// Remove any Range request headers, because the rewrite will mess up any lengths.
		req.Header.Del("Range")
		req.Header.Del("If-Range")

		rewrite = true
	}

	var rewriter *sessionRewriter

	// Keep the current writer for later
	writer := res.Writer

	if rewrite {
		// Put the session rewriter in the middle. This will collect
		// the data that we need to rewrite.
		rewriter = &sessionRewriter{
			ResponseWriter: res.Writer,
		}

		res.Writer = rewriter
	}

	err := next(c)

	// Restore the original writer
	res.Writer = writer

	if err != nil {
		return err
	}

	if rewrite {
		if res.Status < 200 || res.Status >= 300 {
			res.Write(rewriter.buffer.Bytes())
			return nil
		}

		// Rewrite the data befor sending it to the client
		rewriter.rewriteHLS(sessionID, c.Request().URL)

		res.Header().Set("Cache-Control", "private")
		res.Write(rewriter.buffer.Bytes())
	}

	if isM3U8 || isTS {
		if res.Status >= 200 && res.Status < 300 {
			// Collect how many bytes we've written in this session
			h.hlsEgressCollector.Egress(sessionID, headerSize(res.Header()))
			h.hlsEgressCollector.Egress(sessionID, res.Size)

			if isTS {
				// Activate the session. If the session is already active, this is a noop
				h.hlsEgressCollector.Activate(sessionID)
			}
		}
	}

	return nil
}

type bodyReader struct {
	reader io.ReadCloser
	buffer bytes.Buffer
	size   int64
}

func (r *bodyReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	if n > 0 {
		r.buffer.Write(b[:n])
	}
	r.size += int64(n)

	return n, err
}

func (r *bodyReader) Close() error {
	return r.reader.Close()
}

func (r *bodyReader) getSegments(dir string) []string {
	segments := []string{}

	// Find all segment URLs in the .m3u8
	scanner := bufio.NewScanner(&r.buffer)
	for scanner.Scan() {
		line := scanner.Text()

		// Ignore empty lines
		if len(line) == 0 {
			continue
		}

		// Ignore comments
		if strings.HasPrefix(line, "#") {
			continue
		}

		u, err := url.Parse(line)
		if err != nil {
			// Invalid URL
			continue
		}

		if u.Scheme != "" {
			// Ignore full URLs
			continue
		}

		// Ignore anything that doesn't end in .ts
		if !strings.HasSuffix(u.Path, ".ts") {
			continue
		}

		path := u.Path

		if !strings.HasPrefix(u.Path, "/") {
			path = urlpath.Join(dir, u.Path)
		}

		segments = append(segments, path)
	}

	return segments
}

type bodysizeReader struct {
	reader io.ReadCloser
	size   int64
}

func (r *bodysizeReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	r.size += int64(n)

	return n, err
}

func (r *bodysizeReader) Close() error {
	return r.reader.Close()
}

type sessionRewriter struct {
	http.ResponseWriter
	buffer bytes.Buffer
}

func (g *sessionRewriter) Write(data []byte) (int, error) {
	// Write the data into internal buffer for later rewrite
	w, err := g.buffer.Write(data)

	return w, err
}

func (g *sessionRewriter) rewriteHLS(sessionID string, requestURL *url.URL) {
	var buffer bytes.Buffer

	isMaster := false

	// Find all URLS in the .m3u8 and add the session ID to the query string
	scanner := bufio.NewScanner(&g.buffer)
	for scanner.Scan() {
		line := scanner.Text()

		// Write empty lines unmodified
		if len(line) == 0 {
			buffer.WriteString(line + "\n")
			continue
		}

		// Write comments unmodified
		if strings.HasPrefix(line, "#") {
			buffer.WriteString(line + "\n")
			continue
		}

		u, err := url.Parse(line)
		if err != nil {
			buffer.WriteString(line + "\n")
			continue
		}

		// Write anything that doesn't end in .m3u8 or .ts unmodified
		if !strings.HasSuffix(u.Path, ".m3u8") && !strings.HasSuffix(u.Path, ".ts") {
			buffer.WriteString(line + "\n")
			continue
		}

		q := url.Values{}

		for key, values := range requestURL.Query() {
			for _, value := range values {
				q.Add(key, value)
			}
		}

		for key, values := range u.Query() {
			for _, value := range values {
				q.Set(key, value)
			}
		}

		loop := false

		// If this is a master manifest (i.e. an m3u8 which contains references to other m3u8), then
		// we give each substream an own session ID if they don't have already.
		if strings.HasSuffix(u.Path, ".m3u8") {
			// Check if we're referring to ourselves. This will cause an infinite loop
			// and has to be stopped.
			file := u.Path
			if !strings.HasPrefix(file, "/") {
				dir := urlpath.Dir(requestURL.Path)
				file = filepath.Join(dir, file)
			}

			if requestURL.Path == file {
				loop = true
			}

			q.Set("session", shortuuid.New())

			isMaster = true
		} else {
			q.Set("session", sessionID)
		}

		u.RawQuery = q.Encode()

		if loop {
			buffer.WriteString("# m3u8 is referencing itself: " + u.String() + "\n")
		} else {
			buffer.WriteString(u.String() + "\n")
		}
	}

	if err := scanner.Err(); err != nil {
		return
	}

	// If this is not a master manifest and there isn't a session ID, we add a new session ID.
	if !isMaster && len(sessionID) == 0 {
		sessionID = shortuuid.New()

		buffer.Reset()

		buffer.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=1024\n")

		// Add the session ID to the query string
		q := requestURL.Query()
		q.Set("session", sessionID)

		buffer.WriteString(urlpath.Base(requestURL.Path) + "?" + q.Encode())
	}

	g.buffer = buffer
}
