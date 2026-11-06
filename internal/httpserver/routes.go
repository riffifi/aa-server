package httpserver

import (
	"net/http"
	"strings"
)

type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

func (s *Server) handle(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
	method, path, _ := strings.Cut(pattern, " ")
	s.registered = append(s.registered, Route{method, path})
}

// NativeRoutes lists handler registrations without opening a database or sockets.
// Presence here indicates implementation, not a claim of complete behavioral parity.
func NativeRoutes() []Route {
	server := &Server{mux: http.NewServeMux(), config: Config{StaticDir: "."}}
	server.routes()
	return append([]Route(nil), server.registered...)
}
