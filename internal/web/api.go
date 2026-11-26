package web

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"byname/internal/app"
	"byname/internal/link"
	"byname/internal/store"
)

// apiLink is the JSON form of a link.
type apiLink struct {
	Name        string    `json:"name"`
	Target      string    `json:"target"`
	Description string    `json:"description"`
	URL         string    `json:"url"`
	Rev         int64     `json:"rev"`
	Clicks      int64     `json:"clicks"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedBy   string    `json:"updated_by"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// baseURL is the address people use on r's path: the public https:// address
// through Tailscale Serve or Funnel, http://<name> otherwise.
func (s *Server) baseURL(r *http.Request) string {
	if public(r) {
		return s.publicURL
	}
	return "http://" + s.display
}

func (s *Server) toAPI(r *http.Request, l store.Link) apiLink {
	return apiLink{
		Name: l.Name, Target: l.Target, Description: l.Description,
		URL: s.baseURL(r) + "/" + l.Name,
		Rev: l.Rev, Clicks: l.Clicks,
		CreatedBy: l.CreatedBy, CreatedAt: l.CreatedAt.UTC(),
		UpdatedBy: l.UpdatedBy, UpdatedAt: l.UpdatedAt.UTC(),
	}
}

// apiInput is a create or update request. On update, omitted fields keep
// their current values.
type apiInput struct {
	Name        *string `json:"name"`
	Target      *string `json:"target"`
	Description *string `json:"description"`
	Rev         int64   `json:"rev"`
}

func orDefault(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

type apiError struct {
	Error string `json:"error"`
	Field string `json:"field,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *Server) apiFail(w http.ResponseWriter, r *http.Request, err error) {
	status := errStatus(err)
	if status == http.StatusInternalServerError {
		s.log.Error("api request failed", "path", r.URL.Path, "err", err)
		writeJSON(w, status, apiError{Error: "internal error"})
		return
	}
	e := apiError{Error: err.Error()}
	var fe *app.FieldError
	if errors.As(err, &fe) {
		e.Field = fe.Field
	}
	writeJSON(w, status, e)
}

// readJSON requires a JSON content type and decodes the body strictly.
// Requiring application/json also means a browser can only send this request
// cross-origin after a CORS preflight, which this server never approves.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "Content-Type must be application/json"})
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid JSON: " + err.Error()})
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid JSON: trailing data"})
		return false
	}
	return true
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sort := q.Get("sort")
	if !sorts[sort] {
		sort = "name"
	}
	links, err := s.svc.Store.List(r.Context(), store.ListOptions{Query: q.Get("q"), Sort: sort})
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	out := make([]apiLink, 0, len(links))
	for _, l := range links {
		out = append(out, s.toAPI(r, l))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiLookup(w http.ResponseWriter, r *http.Request) (store.Link, bool) {
	name, err := link.ParseName(r.PathValue("name"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, apiError{Error: store.ErrNotFound.Error()})
		return store.Link{}, false
	}
	l, err := s.svc.Store.Get(r.Context(), link.Key(name))
	if err != nil {
		s.apiFail(w, r, err)
		return store.Link{}, false
	}
	return l, true
}

func (s *Server) apiGet(w http.ResponseWriter, r *http.Request) {
	if l, ok := s.apiLookup(w, r); ok {
		writeJSON(w, http.StatusOK, s.toAPI(r, l))
	}
}

func (s *Server) apiCreate(w http.ResponseWriter, r *http.Request) {
	var in apiInput
	if !readJSON(w, r, &in) {
		return
	}
	l, err := s.svc.Create(r.Context(), app.Input{
		Name: orDefault(in.Name, ""), Target: orDefault(in.Target, ""), Description: orDefault(in.Description, ""),
	}, actorFrom(r))
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	w.Header().Set("Location", "/_/api/links/"+l.Name)
	writeJSON(w, http.StatusCreated, s.toAPI(r, l))
}

func (s *Server) apiUpdate(w http.ResponseWriter, r *http.Request) {
	l, ok := s.apiLookup(w, r)
	if !ok {
		return
	}
	var in apiInput
	if !readJSON(w, r, &in) {
		return
	}
	if in.Rev == 0 {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "rev is required (GET the link first)", Field: "rev"})
		return
	}
	updated, err := s.svc.Update(r.Context(), l.ID, in.Rev, app.Input{
		Name:        orDefault(in.Name, l.Name),
		Target:      orDefault(in.Target, l.Target),
		Description: orDefault(in.Description, l.Description),
	}, actorFrom(r))
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.toAPI(r, updated))
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	l, ok := s.apiLookup(w, r)
	if !ok {
		return
	}
	// As for updates, the caller names the revision it means to delete.
	rev, err := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	if err != nil || rev <= 0 {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "rev is required (DELETE …?rev=N; GET the link first)", Field: "rev"})
		return
	}
	if err := s.svc.Delete(r.Context(), l.ID, rev, actorFrom(r)); err != nil {
		s.apiFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Export is the file format of /_/export and `byname export` / `byname import`.
type Export struct {
	Format     string       `json:"format"`
	Version    int          `json:"version"`
	ExportedAt time.Time    `json:"exported_at"`
	Links      []ExportLink `json:"links"`
}

// ExportLink is one exported link.
type ExportLink struct {
	Name        string    `json:"name"`
	Target      string    `json:"target"`
	Description string    `json:"description,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	Clicks      int64     `json:"clicks,omitempty"`
}

// ExportFormat identifies export files.
const ExportFormat = "byname-links"

// BuildExport converts links to the export format.
func BuildExport(links []store.Link, now time.Time) Export {
	e := Export{Format: ExportFormat, Version: 1, ExportedAt: now.UTC(), Links: make([]ExportLink, 0, len(links))}
	for _, l := range links {
		e.Links = append(e.Links, ExportLink{
			Name: l.Name, Target: l.Target, Description: l.Description,
			CreatedBy: l.CreatedBy, CreatedAt: l.CreatedAt.UTC(),
			UpdatedBy: l.UpdatedBy, UpdatedAt: l.UpdatedAt.UTC(),
			Clicks: l.Clicks,
		})
	}
	return e
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	links, err := s.svc.Store.List(r.Context(), store.ListOptions{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	w.Header().Set("Content-Disposition", `attachment; filename="byname-links-`+now.Format("20060102")+`.json"`)
	writeJSON(w, http.StatusOK, BuildExport(links, now))
}

type healthReport struct {
	Status        string     `json:"status"`
	Version       string     `json:"version"`
	UptimeSeconds int64      `json:"uptime_seconds"`
	Links         int        `json:"links"`
	Schema        int        `json:"schema"`
	Power         *powerInfo `json:"power,omitempty"`
	DiskFreeMB    *int64     `json:"disk_free_mb,omitempty"`
	LowDisk       bool       `json:"low_disk,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// lowDiskMB is the free-space level below which health reports low_disk.
const lowDiskMB = 256

func diskFreeMB(dir string) *int64 {
	if dir == "" {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return nil
	}
	mb := int64(uint64(st.Bavail) * uint64(st.Bsize) >> 20)
	return &mb
}

type powerInfo struct {
	Raw                   string `json:"raw"`
	UnderVoltageNow       bool   `json:"under_voltage_now"`
	ThrottledNow          bool   `json:"throttled_now"`
	UnderVoltageSinceBoot bool   `json:"under_voltage_since_boot"`
	ThrottledSinceBoot    bool   `json:"throttled_since_boot"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	rep := healthReport{
		Status:        "ok",
		Version:       s.cfg.Version,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Power:         readThrottled(s.cfg.ThrottledPath),
		DiskFreeMB:    diskFreeMB(s.cfg.DataDir),
	}
	rep.LowDisk = rep.DiskFreeMB != nil && *rep.DiskFreeMB < lowDiskMB
	ctx := r.Context()
	status := http.StatusOK
	var err error
	if err = s.svc.Store.Ping(ctx); err == nil {
		if rep.Links, err = s.svc.Store.CountLive(ctx); err == nil {
			rep.Schema, err = s.svc.Store.SchemaVersion(ctx)
		}
	}
	if err != nil {
		s.log.Error("health check failed", "err", err)
		rep.Status, rep.Error, status = "error", "database unavailable", http.StatusServiceUnavailable
	}
	writeJSON(w, status, rep)
}

// readThrottled parses the Raspberry Pi firmware's throttle bitmask
// (bit 0 under-voltage now, 2 throttled now, 16/18 the same since boot).
func readThrottled(path string) *powerInfo {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(b))
	v, err := strconv.ParseUint(strings.TrimPrefix(raw, "0x"), 16, 32)
	if err != nil {
		return nil
	}
	return &powerInfo{
		Raw:                   "0x" + strconv.FormatUint(v, 16),
		UnderVoltageNow:       v&(1<<0) != 0,
		ThrottledNow:          v&(1<<2) != 0,
		UnderVoltageSinceBoot: v&(1<<16) != 0,
		ThrottledSinceBoot:    v&(1<<18) != 0,
	}
}

type openSearchDesc struct {
	XMLName       xml.Name `xml:"OpenSearchDescription"`
	NS            string   `xml:"xmlns,attr"`
	ShortName     string   `xml:"ShortName"`
	Description   string   `xml:"Description"`
	InputEncoding string   `xml:"InputEncoding"`
	URL           struct {
		Type     string `xml:"type,attr"`
		Method   string `xml:"method,attr"`
		Template string `xml:"template,attr"`
	} `xml:"Url"`
}

func (s *Server) openSearch(w http.ResponseWriter, r *http.Request) {
	d := openSearchDesc{
		NS:            "http://a9.com/-/spec/opensearch/1.1/",
		ShortName:     s.display + "/",
		Description:   s.display + "/ short links",
		InputEncoding: "UTF-8",
	}
	d.URL.Type, d.URL.Method = "text/html", "get"
	d.URL.Template = s.baseURL(r) + "/_/go?q={searchTerms}"
	w.Header().Set("Content-Type", "application/opensearchdescription+xml")
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(d)
}
