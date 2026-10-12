// Package srv is the addon's HTTP side: what the panel calls (mode http of protocol
// 2). It serves /v2/info (the host checks id and protocol after the install), the
// admin pages under /ui/ (the panel proxies them into a sandboxed frame) and
// /v2/ui/rpc (the frame's calls, forwarded by the panel). Everything is behind the
// Bearer token from MIKAN_ADAPTER_TOKEN; the addon serves nothing else and listens
// only where MIKAN_ADAPTER_LISTEN says (loopback, or the bridge interface that the
// host publishes on loopback).
package srv

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// MaxBody is the largest request body (the panel sends at most 1 MiB itself).
const MaxBody = 1 << 20

// RPCTimeout bounds one call of the pages; the panel itself waits 40 s.
const RPCTimeout = 35 * time.Second

// Info is the addon's description of itself, as /v2/info answers it.
type Info struct {
	ID          string            `json:"id"`
	Protocol    int               `json:"protocol"`
	Version     string            `json:"version"`
	Kind        string            `json:"kind"`
	Mode        string            `json:"mode"`
	Name        map[string]string `json:"name"`
	Description map[string]string `json:"description"`
	Permissions []string          `json:"permissions"`
	UI          UI                `json:"ui"`
	Settings    []any             `json:"settings"` // the page keeps its own settings; the panel draws none
	Help        map[string]string `json:"help"`
}

// UI is where the addon's pages start.
type UI struct {
	Entry string            `json:"entry"`
	Title map[string]string `json:"title"`
}

// Server serves the protocol's addon side.
type Server struct {
	tokenSum  [sha256.Size]byte
	info      Info
	pages     fs.FS // the embedded web/ (index.html, app.js, style.css)
	etags     map[string]string
	rpc       *RPC
	log       *slog.Logger
	startedAt time.Time
	sem       chan struct{} // concurrent RPC calls
}

// Deps wires the server to the rest of the addon.
type Deps struct {
	Info  Info
	Pages fs.FS
	RPC   *RPC
	Log   *slog.Logger
	Token string
}

// New is a Server; the token is MIKAN_ADAPTER_TOKEN.
func New(d Deps) (*Server, error) {
	if d.Token == "" {
		return nil, errors.New("srv: MIKAN_ADAPTER_TOKEN is required")
	}
	s := &Server{
		tokenSum: sha256.Sum256([]byte(d.Token)),
		info:     d.Info,
		pages:    d.Pages,
		etags:    map[string]string{},
		rpc:      d.RPC,
		log:      d.Log,
		sem:      make(chan struct{}, 32),
	}
	s.startedAt = time.Now()
	if err := s.hashPages(); err != nil {
		return nil, err
	}
	return s, nil
}

// hashPages precomputes the ETags of the embedded files (they never change at run
// time, and the frame revalidates through the panel's proxy).
func (s *Server) hashPages() error {
	return fs.WalkDir(s.pages, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(s.pages, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		s.etags[p] = fmt.Sprintf(`"%x"`, sum[:8])
		return nil
	})
}

// Listen is the address check the host's state demands of a protocol 2 addon: a
// plain ip:port. The custom-image addon listens on all of its own interfaces (the
// host publishes the port on loopback alone); the check here is only of the shape.
func Listen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("MIKAN_ADAPTER_LISTEN %q: want ip:port", addr)
	}
	return nil
}

// Handler is the addon's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/info", s.handleInfo)
	mux.HandleFunc("GET /v2/health", s.handleHealth)
	mux.HandleFunc("POST /v2/check", s.handleCheck)
	mux.HandleFunc("GET /ui/{file...}", s.handleUI)
	mux.HandleFunc("POST /v2/ui/rpc", s.handleRPC)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.auth(mux)
}

// auth is the Bearer check in front of everything: the panel calls the addon with
// MIKAN_ADAPTER_TOKEN, and nobody else may read its pages or move its tickets. The
// comparison is constant time (hashed, so the length leaks nothing).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
			unauthorized(w)
			return
		}
		got := sha256.Sum256([]byte(h[len(prefix):]))
		if subtle.ConstantTimeCompare(got[:], s.tokenSum[:]) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="mikan-addon"`)
	writeErr(w, http.StatusUnauthorized, "unauthorized", "a valid Bearer token is required")
}

// apiError is the {"code","message"} shape the protocol uses.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.info)
}

// handleHealth answers the panel's (future) liveness call with what the addon knows
// of itself; it never fails, so a restart loop is visible in the log, not here.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.info.Version,
		"uptime_s": int64(time.Since(s.startedAt).Seconds())})
}

// handleCheck answers the panel's (future) settings check: the addon keeps its own
// settings, so there is nothing to refuse.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, MaxBody))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUI serves one embedded file of the pages. The panel forwards Accept,
// Accept-Language, If-None-Match and If-Modified-Since (nothing that carries the
// admin's session) and passes back only Content-Type, Cache-Control, ETag and
// Last-Modified — so those are the only headers set here.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	// The mux pattern already refuses an empty name; refuse traversal shapes too,
	// even though the embedded FS could not leave its root anyway.
	name = path.Clean("/" + name)
	name = strings.TrimPrefix(name, "/")
	if name == "" || name == "." || strings.Contains(name, "..") {
		writeErr(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	data, err := fs.ReadFile(s.pages, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	etag := s.etags[name]
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// rpcCall is the panel's forwarding of a frame's call.
type rpcCall struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
	Admin  struct {
		Lang string `json:"lang"`
	} `json:"admin"`
}

// handleRPC is the frame's way to the addon: the panel forwards {method, path, body,
// admin} and passes the answer's status and JSON back as they are. The addon's own
// HTTP status is the frame's status, so a refusal here is a real 4xx/5xx there.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	var call rpcCall
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "the body is not a valid RPC call")
		return
	}
	if !validRPCPath(call.Path) || !validRPCMethod(call.Method) {
		writeErr(w, http.StatusBadRequest, "bad_request", "method is GET, POST, PUT or DELETE and path starts with /")
		return
	}
	// One semaphore for all calls: the frame itself keeps 8 in flight, and a runaway
	// page must not exhaust the addon (the container has 64 processes' room and one
	// CPU's patience).
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "the addon is shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), RPCTimeout)
	defer cancel()

	status, body := s.rpc.Handle(ctx, call.Method, call.Path, call.Body, call.Admin.Lang)
	if body == nil {
		body = struct{}{}
	}
	data, err := json.Marshal(body)
	if err != nil || len(data) > MaxBody {
		s.log.Error("rpc: the answer did not marshal or is too large", "path", call.Path, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "the answer is broken")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func validRPCMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// validRPCPath mirrors the panel's own check: starts with /, at most 1024, no
// control characters.
func validRPCPath(p string) bool {
	if !strings.HasPrefix(p, "/") || len(p) > 1024 {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// Run serves h on addr until ctx ends, then lets the requests in flight finish.
func Run(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// An RPC may wait for the LLM (35 s) — the write side outlives it.
		WriteTimeout:   RPCTimeout + 15*time.Second,
		IdleTimeout:    time.Minute,
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	log.Info("serving", "listen", addr)
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}
