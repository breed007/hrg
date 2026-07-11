package web

import (
	"net/http"

	"github.com/breed007/hrg/internal/netmap"
	"github.com/breed007/hrg/internal/store"
)

func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	resources, err := s.store.ListResources(r.Context(), store.ListFilter{})
	if err != nil {
		s.fail(w, err)
		return
	}
	edges, err := s.store.ListEdges(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	src := netmap.Mermaid(resources, edges, netmap.MermaidOptions{Links: true})
	s.render(w, "map", "layout", map[string]any{
		"Title": "Network map", "Source": src, "Empty": src == "",
	})
}
