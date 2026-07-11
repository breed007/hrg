package web

import (
	"net/http"

	"github.com/breed007/hrg/internal/netmap"
	"github.com/breed007/hrg/internal/store"
)

func (s *Server) handleIPPlan(w http.ResponseWriter, r *http.Request) {
	resources, err := s.store.ListResources(r.Context(), store.ListFilter{})
	if err != nil {
		s.fail(w, err)
		return
	}
	prefixes, ips := netmap.Reconcile(resources)
	s.render(w, "ipplan", "layout", map[string]any{
		"Title": "IP plan", "Prefixes": prefixes, "IPs": ips,
		"HaveNetbox": netmap.HaveAuthority(resources),
	})
}
