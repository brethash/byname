package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"byname/internal/app"
	"byname/internal/link"
	"byname/internal/store"
)

// follow handles me/<name>[/<rest>] (redirect) and me/<name>+ (preview).
func (s *Server) follow(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	nameEsc, rest, _ := strings.Cut(p, "/")
	raw, err := url.PathUnescape(nameEsc)
	if err != nil {
		s.notFound(w, r, nameEsc, link.ErrNameInvalid)
		return
	}
	preview := false
	if rest == "" && strings.HasSuffix(raw, "+") {
		preview = true
		raw = strings.TrimSuffix(raw, "+")
	}
	name, err := link.ParseName(raw)
	if err != nil {
		s.notFound(w, r, raw, err)
		return
	}
	l, err := s.svc.Store.Get(r.Context(), link.Key(name))
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, name, nil)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if preview {
		s.detail(w, r, l, "")
		return
	}
	s.redirect(w, r, l, rest, r.URL.RawQuery)
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, l store.Link, rest, rawQuery string) {
	dest, err := s.svc.Destination(l, rest, rawQuery)
	if err != nil {
		s.render(w, r, http.StatusBadRequest, "error", view{
			Title: "Can't build that destination",
			Error: err.Error(),
			Link:  &l,
		})
		return
	}
	if r.Method == http.MethodGet {
		s.svc.RecordClick(l.ID)
	}
	// 302, never 301: browsers cache 301s forever, so an edited link would
	// keep sending people to the old destination.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", dest)
	w.WriteHeader(http.StatusFound)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, name string, nameErr error) {
	v := view{Title: "Not found", Missing: trunc(name, 80)}
	v.Form = formData{Name: v.Missing}
	if nameErr != nil {
		v.Form.Errors = map[string]string{"name": nameErr.Error()}
	} else if k := link.Key(name); k != "" {
		v.Suggestions, _ = s.svc.Store.List(r.Context(), store.ListOptions{Query: name, Limit: 5})
	}
	s.render(w, r, http.StatusNotFound, "notfound", v)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var (
		v   = view{Title: "Home"}
		err error
	)
	if v.Popular, err = s.svc.Store.List(ctx, store.ListOptions{Sort: "popular", Limit: 10}); err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Newest, err = s.svc.Store.List(ctx, store.ListOptions{Sort: "created", Limit: 10}); err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Events, err = s.svc.Store.RecentEvents(ctx, 8); err != nil {
		s.fail(w, r, err)
		return
	}
	if v.Count, err = s.svc.Store.CountLive(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "home", v)
}

var sorts = map[string]bool{"name": true, "recent": true, "popular": true, "created": true}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v := view{
		Title:       "All links",
		Query:       strings.TrimSpace(q.Get("q")),
		Sort:        q.Get("sort"),
		ShowDeleted: q.Get("deleted") == "1",
	}
	if !sorts[v.Sort] {
		v.Sort = "name"
	}
	if v.ShowDeleted {
		v.Title = "Deleted links"
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 || page > maxListPage {
		page = 1
	}
	// One extra row tells whether there is a next page.
	links, err := s.svc.Store.List(r.Context(), store.ListOptions{
		Query: v.Query, Sort: v.Sort, Deleted: v.ShowDeleted, Limit: listPageSize + 1, Offset: (page - 1) * listPageSize,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(links) > listPageSize {
		links = links[:listPageSize]
		v.NextPage = page + 1
	}
	if page > 1 {
		v.PrevPage = page - 1
	}
	if len(links) > 0 {
		v.First = (page-1)*listPageSize + 1
		v.Last = v.First + len(links) - 1
	}
	v.Links = links
	s.render(w, r, http.StatusOK, "list", v)
}

// The list page shows this many links at a time, and pages no further than
// maxListPage (past MaxLinks at the default cap).
const (
	listPageSize = 1000
	maxListPage  = 1000
)

// historyPageSize is how many history entries a link's page shows at once;
// older ones are a link away.
const historyPageSize = 50

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	ev, err := s.svc.Store.RecentEvents(r.Context(), 200)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "activity", view{Title: "Activity", Events: ev})
}

func (s *Server) newForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.render(w, r, http.StatusOK, "form", view{
		Title: "New link",
		Form:  formData{Name: trunc(q.Get("name"), link.MaxNameLen), Target: trunc(q.Get("target"), link.MaxTargetLen)},
	})
}

// parseForm reads the form body, answering 413 when it exceeds the body cap.
func (s *Server) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.renderError(w, r, http.StatusRequestEntityTooLarge, "Too large", "That submission is larger than this form accepts.")
		} else {
			s.renderError(w, r, http.StatusBadRequest, "Bad request", "The form could not be read.")
		}
		return false
	}
	return true
}

func formInput(r *http.Request) app.Input {
	return app.Input{
		Name:        r.PostFormValue("name"),
		Target:      r.PostFormValue("target"),
		Description: r.PostFormValue("description"),
	}
}

// formError re-renders a form with the user's input and the error.
func (s *Server) formError(w http.ResponseWriter, r *http.Request, fd formData, err error) {
	status := errStatus(err)
	if status == http.StatusInternalServerError {
		s.fail(w, r, err)
		return
	}
	fd.Errors = map[string]string{}
	var fe *app.FieldError
	if errors.As(err, &fe) {
		fd.Errors[fe.Field] = fe.Error()
	} else {
		fd.Errors["form"] = err.Error()
	}
	title := "New link"
	if fd.Editing {
		title = "Edit " + s.display + "/" + fd.Name
	}
	s.render(w, r, status, "form", view{Title: title, Form: fd})
}

func linkPath(name string) string { return "/" + url.PathEscape(name) + "+" }

func idPath(id int64) string { return "/_/links/" + strconv.FormatInt(id, 10) }

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	if !s.parseForm(w, r) {
		return
	}
	in := formInput(r)
	l, err := s.svc.Create(r.Context(), in, actorFrom(r))
	if err != nil {
		s.formError(w, r, formData{Name: in.Name, Target: in.Target, Description: in.Description}, err)
		return
	}
	http.Redirect(w, r, linkPath(l.Name), http.StatusSeeOther)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) detailByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such link.")
		return
	}
	l, err := s.svc.Store.GetByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.detail(w, r, l, "")
}

// detail shows a link and a page of its history: the newest entries, or
// with ?before=<entry id> the ones older than that.
func (s *Server) detail(w http.ResponseWriter, r *http.Request, l store.Link, msg string) {
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || before < 0 {
		before = 0
	}
	hist, err := s.svc.Store.History(r.Context(), l.ID, before, historyPageSize+1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := view{Title: s.display + "/" + l.Name, Link: &l, Message: msg, OlderHistory: before > 0}
	if len(hist) > historyPageSize {
		hist = hist[:historyPageSize]
		v.OlderThan = hist[len(hist)-1].ID
	}
	v.Events = hist
	s.render(w, r, http.StatusOK, "detail", v)
}

func (s *Server) editForm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such link.")
		return
	}
	l, err := s.svc.Store.GetByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if l.Deleted() {
		s.renderError(w, r, http.StatusConflict, "Link is deleted", "Restore the link before editing it.")
		return
	}
	s.render(w, r, http.StatusOK, "form", view{Title: "Edit " + s.display + "/" + l.Name, Form: formData{
		ID: l.ID, Rev: l.Rev, Name: l.Name, Target: l.Target, Description: l.Description, Editing: true,
	}})
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such link.")
		return
	}
	if !s.parseForm(w, r) {
		return
	}
	rev, _ := strconv.ParseInt(r.PostFormValue("rev"), 10, 64)
	in := formInput(r)
	l, err := s.svc.Update(r.Context(), id, rev, in, actorFrom(r))
	if err != nil {
		s.formError(w, r, formData{ID: id, Rev: rev, Name: in.Name, Target: in.Target, Description: in.Description, Editing: true}, err)
		return
	}
	http.Redirect(w, r, linkPath(l.Name), http.StatusSeeOther)
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such link.")
		return
	}
	if !s.parseForm(w, r) {
		return
	}
	rev, _ := strconv.ParseInt(r.PostFormValue("rev"), 10, 64)
	if err := s.svc.Delete(r.Context(), id, rev, actorFrom(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, idPath(id), http.StatusSeeOther)
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such link.")
		return
	}
	l, err := s.svc.Restore(r.Context(), id, actorFrom(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, linkPath(l.Name), http.StatusSeeOther)
}

func (s *Server) revert(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such history entry.")
		return
	}
	l, err := s.svc.RevertTo(r.Context(), id, actorFrom(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, linkPath(l.Name), http.StatusSeeOther)
}

// keyword serves browser search-keyword queries: "me wiki some page" arrives
// as /_/go?q=wiki+some+page and goes to me/wiki/some/page; anything that is
// not a link becomes a search.
func (s *Server) keyword(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	words := strings.Fields(q)
	first := strings.TrimPrefix(words[0], s.display+"/")
	parts := strings.Split(first, "/")
	segs := append(parts[1:], words[1:]...)
	if name, err := link.ParseName(parts[0]); err == nil {
		if l, err := s.svc.Store.Get(r.Context(), link.Key(name)); err == nil {
			esc := make([]string, 0, len(segs))
			for _, sgm := range segs {
				if sgm != "" {
					esc = append(esc, url.PathEscape(sgm))
				}
			}
			s.redirect(w, r, l, strings.Join(esc, "/"), "")
			return
		}
	}
	http.Redirect(w, r, "/_/links?q="+url.QueryEscape(q), http.StatusFound)
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	if sc := sessionOf(r); sc != nil {
		a, _ := s.actor(r)
		x := sc.Session
		s.render(w, r, http.StatusOK, "whoami", view{Title: "Who am I", Session: &x,
			Provider: strings.TrimPrefix(a.Node, "via "), PeerIP: a.IP, Public: true})
		return
	}
	a, id, err := s.identify(r)
	ip, _ := peerIP(r)
	v := view{Title: "Who am I", Identity: id, PeerIP: a.IP, OnTailnet: onTailnet(ip), Public: public(r)}
	if err != nil && v.OnTailnet {
		v.WhoIsErr = err.Error()
	}
	s.render(w, r, http.StatusOK, "whoami", v)
}
