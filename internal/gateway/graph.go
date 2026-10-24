package gateway

import (
	"net/http"
	"net/url"

	"github.com/hingst-it/panko/internal/graph"
	"github.com/hingst-it/panko/internal/jsonx"
)

func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	team, version, plural, name := r.PathValue("team"), r.PathValue("version"), r.PathValue("plural"), r.PathValue("name")
	if !s.authorize(w, r, team) {
		return
	}
	discovered, err := s.Discovery.Get(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	resource, ok := discovered[Route{version, plural}]
	if !ok {
		fail(w, 404, "No exposed resource at this route", nil)
		return
	}
	path := "/apis/" + url.PathEscape(resource.Group) + "/" + url.PathEscape(version) + "/namespaces/" + url.PathEscape(team) + "/" + url.PathEscape(plural) + "/" + url.PathEscape(name)
	current, err := s.API.Request(r.Context(), "GET", path, nil, "")
	if err != nil {
		respondError(w, err)
		return
	}
	uid := jsonx.Str(jsonx.Obj(current["metadata"])["uid"])
	statusIdentity := jsonx.Str(jsonx.Obj(current["status"])["identity"])
	identity := statusIdentity
	if identity == "" {
		identity = uid
	}
	cm, err := s.API.Request(r.Context(), "GET", "/api/v1/namespaces/"+url.PathEscape(team)+"/configmaps/"+url.PathEscape("panko-graph-"+identity), nil, "")
	if err != nil {
		respondError(w, err)
		return
	}
	a, err := graph.Parse([]byte(jsonx.Str(jsonx.Obj(cm["data"])["graph.json"])))
	if err != nil || a.Identity != identity || (a.UID != uid && statusIdentity != a.Identity) {
		fail(w, 404, "Graph unavailable", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, a)
}
