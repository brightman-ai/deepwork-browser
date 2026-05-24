package dwbrowser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// Server provides browser automation as an embeddable HTTP service.
//
// Usage (embedded):
//
//	srv, _ := dwbrowser.NewServer(dwbrowser.WithAddr(":8033"))
//	mux.Handle("/browser/", http.StripPrefix("/browser", srv.Handler()))
//
// Usage (standalone):
//
//	srv, _ := dwbrowser.NewServer()
//	srv.ListenAndServe(ctx)
type Server struct {
	mux      *http.ServeMux
	pool     *browser.BrowserPool
	hooks    Hooks
	config   Config
	listener net.Listener
	mu       sync.Mutex
}

// NewServer creates a Server with the provided options.
func NewServer(opts ...Option) (*Server, error) {
	s := &Server{config: DefaultConfig()}
	for _, o := range opts {
		o(s)
	}
	s.mux = http.NewServeMux()
	s.registerRoutes()
	return s, nil
}

// Handler returns an http.Handler for embedding into an existing mux.
// Routes are relative (no /api/ prefix); the caller may add a prefix via
// http.StripPrefix if needed.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts a standalone HTTP server on s.config.Addr.
// It blocks until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	root := http.NewServeMux()
	root.Handle("/api/", http.StripPrefix("/api", s.mux))

	ln, err := net.Listen("tcp", s.config.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.config.Addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	srv := &http.Server{Handler: root}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	return srv.Serve(ln)
}

// Pool returns the underlying BrowserPool for programmatic access.
// May return nil if the pool has not been initialized yet.
func (s *Server) Pool() *browser.BrowserPool { return s.pool }

// Port returns the TCP port the server is listening on.
// Returns 0 if ListenAndServe has not been called.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return 0
	}
	return s.listener.Addr().(*net.TCPAddr).Port
}

// Close shuts down the server and the underlying browser pool.
func (s *Server) Close() error {
	if s.pool != nil {
		s.pool.Shutdown(context.Background())
	}
	s.mu.Lock()
	ln := s.listener
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	return nil
}

// registerRoutes wires the built-in HTTP routes.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /status", s.handleStatus)
	s.mux.HandleFunc("POST /navigate", s.handleNavigate)
	s.mux.HandleFunc("GET /tabs", s.handleTabs)
	// LiveView WebSocket route will be added in a future release.
}

// handleStatus returns a basic health-check response.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok"}`)
}

// handleNavigate is a placeholder for the navigate endpoint.
func (s *Server) handleNavigate(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// handleTabs returns the list of active tabs.
func (s *Server) handleTabs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"tabs":[]}`)
}
