package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var uiFS embed.FS

// server holds the interface's shared state.
type server struct {
	cfg *Config

	// The candidate list is derived from the index and costs real work at 1500+
	// records. Since the interface polls once a second, the result is cached
	// against the index file's modification stamp.
	mu       sync.Mutex
	candMod  time.Time
	candSize int64
	cands    []Candidate

	// scan is the doctor's library sweep. It outlives a request, so it lives on
	// the server rather than in a handler.
	scan scanner
}

func cmdServe(ctx context.Context, cfg *Config, args []string) error {
	// Note: the local name is "flags"; "fs" belongs to the io/fs package here.
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", ":7654", "address to listen on (:7654 = all interfaces)")
	token := flags.String("token", "", "optional access key (?token=... or Authorization: Bearer)")
	open := flags.Bool("open", false, "open in a browser")
	_ = flags.Parse(args)

	s := &server{cfg: cfg}
	mux := http.NewServeMux()
	s.routes(mux)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", *addr, err)
	}

	local := isLoopback(*addr)
	info("vtrans interface: %s", "http://"+friendlyAddr(ln.Addr().String()))
	if !local {
		for _, u := range lanURLs(ln.Addr().String()) {
			info("  %s", u)
		}
		if *token == "" {
			warn("This address is reachable over the network and has no authentication:")
			warn("  anyone who can see the interface can change the configuration and empty the trash.")
			warn("  To close it: vtrans serve --token <key>   (or --addr 127.0.0.1:7654)")
		}
	}
	if *open {
		go browserOpen("http://" + friendlyAddr(ln.Addr().String()))
	}

	var h http.Handler = mux
	h = requireToken(h, *token)
	// The DNS rebinding guard only makes sense when listening locally; over the
	// network requests arrive by machine name and restricting Host would break access.
	if local {
		h = guardHost(h)
	}

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *server) routes(mux *http.ServeMux) {
	sub, _ := fs.Sub(uiFS, "ui")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/queue", s.handleQueue)
	mux.HandleFunc("GET /api/done", s.handleDone)
	mux.HandleFunc("GET /api/failed", s.handleFailed)
	mux.HandleFunc("POST /api/failed/clear", s.handleFailedClear)
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("POST /api/config", s.handleConfigPost)
	mux.HandleFunc("POST /api/trash/empty", s.handleTrashEmpty)
	mux.HandleFunc("GET /api/trash", s.handleTrashList)
	mux.HandleFunc("POST /api/trash/restore", s.handleTrashRestore)
	mux.HandleFunc("POST /api/trash/delete", s.handleTrashDelete)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/ignored", s.handleIgnored)
	mux.HandleFunc("POST /api/ignore", s.handleIgnore)
	mux.HandleFunc("POST /api/unignore", s.handleUnignore)
	mux.HandleFunc("GET /api/library", s.handleLibrary)
	mux.HandleFunc("GET /api/art", s.handleArt)
	mux.HandleFunc("GET /api/doctor/file", s.handleDoctorFile)
	mux.HandleFunc("GET /api/doctor/scan", s.handleDoctorScanGet)
	mux.HandleFunc("POST /api/doctor/scan", s.handleDoctorScanStart)
	mux.HandleFunc("POST /api/doctor/scan/stop", s.handleDoctorScanStop)
}

// guardHost is a simple shield against DNS rebinding. It is only active when
// listening locally (see cmdServe).
//
// Binding to 127.0.0.1 is not enough on its own: a malicious page can resolve
// its own domain to 127.0.0.1 and reach this server through the browser. Such a
// request carries the attacker's domain in the Host header, so allowing only
// localhost and bare IP names cuts the attack off.
func guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "localhost" && net.ParseIP(host) == nil {
			http.Error(w, "invalid Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireToken validates every request when a key is configured.
//
// A constant-time comparison is used: ordinary string equality returns at the
// first differing byte, and the measurable timing difference would let the key
// be guessed character by character.
func requireToken(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("token")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			http.Error(w, "invalid key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// lanURLs lists the machine's network addresses in clickable form.
func lanURLs(listen string) []string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		out = append(out, fmt.Sprintf("http://%s:%s", ipn.IP, port))
	}
	sort.Strings(out)
	return out
}

func isLoopback(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if h == "" {
		return false // ":7654" means all interfaces
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// friendlyAddr turns unconnectable addresses such as 0.0.0.0 into clickable ones.
func friendlyAddr(a string) string {
	h, p, err := net.SplitHostPort(a)
	if err != nil {
		return a
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		return "127.0.0.1:" + p
	}
	return a
}

func browserOpen(url string) {
	for _, bin := range []string{"xdg-open", "open"} {
		if err := runQuiet(bin, url); err == nil {
			return
		}
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The header has already gone out; logging is all that is left.
		warn("could not write the response: %v", err)
	}
}

func httpErr(w http.ResponseWriter, code int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, a...)})
}

func intParam(r *http.Request, name string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v < 0 {
		return def
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

// candidates produces the candidate list from the cache or from the index.
func (s *server) candidates() []Candidate {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := os.Stat(indexPath())
	if err == nil && s.cands != nil &&
		st.ModTime().Equal(s.candMod) && st.Size() == s.candSize {
		return s.cands
	}

	ix := LoadIndex()
	s.cands = BuildCandidates(s.cfg, ix)
	if err == nil {
		s.candMod, s.candSize = st.ModTime(), st.Size()
	}
	return s.cands
}

// --- state ---

type stateResp struct {
	Run     *RunState  `json:"run"`
	Running bool       `json:"running"`
	Summary summaryDTO `json:"summary"`
	Queue   queueStat  `json:"queue"`
	// Skipped is what sits in the library matching the rules but will not be
	// attempted: a permanent failure or an entry on the ignore list.
	Skipped queueStat `json:"skipped"`
	Trash   trashStat `json:"trash"`
	Notices []string  `json:"notices"`
	Mode    string    `json:"mode"`
	// Service is what systemd says about the worker unit, so an empty status
	// panel can explain itself.
	Service ServiceState `json:"service"`
}

type summaryDTO struct {
	Files    int   `json:"files"`
	Replaced int   `json:"replaced"`
	SrcSize  int64 `json:"src_size"`
	OutSize  int64 `json:"out_size"`
	Saved    int64 `json:"saved"`
	Failed   int   `json:"failed"`
	Ignored  int   `json:"ignored"`
}

type queueStat struct {
	Files int   `json:"files"`
	Size  int64 `json:"size"`
}

type trashStat struct {
	Dir   string `json:"dir"`
	Files int    `json:"files"`
	Size  int64  `json:"size"`
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	rs := LoadRunState()
	alive := rs.Alive()
	if !alive {
		// A stale record must not look "running" in the interface, but throwing the
		// last state away entirely would lose information: the record is kept, the
		// flag is cleared.
		rs = nil
	}

	recs, _ := loadDone()
	var sum summaryDTO
	for _, rec := range recs {
		sum.Files++
		sum.SrcSize += rec.SrcSize
		sum.OutSize += rec.DestSize
		if rec.Replaced {
			sum.Replaced++
		}
	}
	sum.Saved = sum.SrcSize - sum.OutSize
	sum.Failed = len(LoadFailed())
	sum.Ignored = len(loadIgnoredRecords())

	// The queue counts only what will actually be attempted. Files carrying a
	// permanent failure record or sitting on the ignore list are counted apart:
	// mixed in, they made the queue look stuck, since no amount of running ever
	// brought the number down.
	qs, skipped := s.queueSplit()

	var notices []string
	if op, err := RecoverJournal(); err == nil && op != nil {
		notices = append(notices, fmt.Sprintf(
			"There is a half-finished operation (%s). vtrans refuses to run until it is checked by hand.",
			filepath.Base(op.Src)))
	}
	if s.cfg.Mode == ModeReplace && s.cfg.TrashDir == "" {
		notices = append(notices, "The trash directory is empty: in replace mode originals are deleted outright, with no way back.")
	}

	writeJSON(w, stateResp{
		Run: rs, Running: alive, Summary: sum, Queue: qs, Skipped: skipped,
		Trash: s.trashStat(), Notices: notices, Mode: string(s.cfg.Mode),
		Service: readServiceState(r.Context(), workerUnit),
	})
}

// queueSplit divides the candidates into those that will be attempted and those
// that will not.
func (s *server) queueSplit() (queue, skipped queueStat) {
	failed := LoadFailed()
	ignored := LoadIgnored()
	for _, c := range s.candidates() {
		if failed[c.Path] || ignored[c.Path] {
			skipped.Files++
			skipped.Size += c.Size
			continue
		}
		queue.Files++
		queue.Size += c.Size
	}
	return queue, skipped
}

// workerUnit is the systemd unit doing the encoding. Asking systemd about it is
// what lets the interface tell "waiting for the next cycle" apart from "not
// running at all" - without it both look like an empty status panel.
const workerUnit = "vtrans.service"

// handleLogs serves the working side's journal.
func (s *server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := intParam(r, "n", 300, 2000)
	lines, err := readJournal(r.Context(), workerUnit, n)
	if err != nil {
		// Reading the journal needs membership of the systemd-journal or adm
		// group. Say so instead of showing an empty box.
		httpErr(w, http.StatusServiceUnavailable,
			"could not read the journal (is the user in the adm or systemd-journal group?): %v", err)
		return
	}
	writeJSON(w, map[string]any{"unit": workerUnit, "lines": lines})
}

func (s *server) trashStat() trashStat {
	ts := trashStat{Dir: s.cfg.TrashDir}
	if ts.Dir == "" {
		return ts
	}
	_ = filepath.WalkDir(ts.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if st, err := d.Info(); err == nil {
			ts.Files++
			ts.Size += st.Size()
		}
		return nil
	})
	return ts
}

// --- queue ---

type queueItem struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Codec    string  `json:"codec"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	TargetW  int     `json:"target_w"`
	TargetH  int     `json:"target_h"`
	Quality  int     `json:"quality"`
	IsTV     bool    `json:"is_tv"`
	Duration float64 `json:"duration"`
	EstSize  int64   `json:"est_size"`
}

func (s *server) handleQueue(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))

	// Files that will not be attempted are left out: this tab answers "what is
	// coming next", and they never are. They are listed under Failures and
	// Ignored instead.
	failed := LoadFailed()
	ignored := LoadIgnored()

	var filtered []Candidate
	for _, c := range s.candidates() {
		if failed[c.Path] || ignored[c.Path] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Path), q) {
			continue
		}
		filtered = append(filtered, c)
	}

	// Largest files first: most of the saving is there, and those are what the
	// user wants to look at first.
	sorted := make([]Candidate, len(filtered))
	copy(sorted, filtered)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Size > sorted[j].Size })

	offset := intParam(r, "offset", 0, 0)
	limit := intParam(r, "limit", 100, 500)
	if offset > len(sorted) {
		offset = len(sorted)
	}
	end := offset + limit
	if end > len(sorted) {
		end = len(sorted)
	}

	items := make([]queueItem, 0, end-offset)
	for _, c := range sorted[offset:end] {
		items = append(items, queueItem{
			Path: c.Path, Name: filepath.Base(c.Path), Size: c.Size,
			Codec: c.Codec, Width: c.Width, Height: c.Height,
			TargetW: c.TargetW, TargetH: c.TargetH, Quality: c.Quality,
			IsTV: c.IsTV, Duration: c.Duration, EstSize: estimateOutSize(s.cfg, c),
		})
	}
	writeJSON(w, map[string]any{"total": len(sorted), "items": items})
}

// estimateOutSize is the single-file form of the plan command's estimate model.
func estimateOutSize(cfg *Config, c Candidate) int64 {
	base := estimatedVideoMbps(cfg, c.Quality)
	px := float64(c.TargetW * c.TargetH)
	estVideo := base * (px / (1920 * 1080)) * 1e6 * c.Duration / 8
	estAudio := estimatedAudio(cfg, c) * c.Duration / 8
	return int64(estVideo + estAudio)
}

// --- completed / failed ---

func (s *server) handleDone(w http.ResponseWriter, r *http.Request) {
	recs, _ := loadDone()
	// Newest first.
	sort.Slice(recs, func(i, j int) bool { return recs[i].At.After(recs[j].At) })
	limit := intParam(r, "limit", 50, 500)
	if limit < len(recs) {
		recs = recs[:limit]
	}
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, map[string]any{
			"path": rec.Src, "name": filepath.Base(rec.Src),
			"src_size": rec.SrcSize, "out_size": rec.DestSize,
			"saved": rec.SrcSize - rec.DestSize, "replaced": rec.Replaced, "at": rec.At,
		})
	}
	writeJSON(w, out)
}

func (s *server) handleFailed(w http.ResponseWriter, r *http.Request) {
	recs := loadFailedRecords()
	sort.Slice(recs, func(i, j int) bool { return recs[i].At.After(recs[j].At) })
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, map[string]any{
			"path": rec.Src, "name": filepath.Base(rec.Src),
			"reason": rec.Reason, "at": rec.At,
		})
	}
	writeJSON(w, out)
}

// --- library ---

// libItem is one row of the library view: what the file is, and where vtrans
// stands with it.
type libItem struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Dir      string  `json:"dir"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration"`
	Codec    string  `json:"codec"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	// State is what the interface colours the card by: done, queued, failed,
	// ignored or skipped (in the library but outside the rules).
	State  string `json:"state"`
	Saved  int64  `json:"saved,omitempty"`
	HasArt bool   `json:"has_art"`
}

func (s *server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	ix := LoadIndex()

	// Every state the interface can show, gathered once rather than per file.
	done := map[string]DoneRecord{}
	if recs, err := loadDone(); err == nil {
		for _, rec := range recs {
			done[rec.Src] = rec
		}
	}
	failed := LoadFailed()
	ignored := LoadIgnored()
	queued := map[string]bool{}
	for _, c := range s.candidates() {
		if !failed[c.Path] && !ignored[c.Path] {
			queued[c.Path] = true
		}
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	wantState := r.URL.Query().Get("state")
	wantCodec := r.URL.Query().Get("codec")

	var items []libItem
	counts := map[string]int{}
	codecs := map[string]int{}

	for _, e := range ix.Entries {
		it := libItem{
			Path: e.Path, Name: filepath.Base(e.Path),
			Dir:  filepath.Base(filepath.Dir(e.Path)),
			Size: e.Size, Duration: e.Duration,
			Codec: e.Codec, Width: e.Width, Height: e.Height,
		}
		switch {
		case failed[e.Path]:
			it.State = "failed"
		case ignored[e.Path]:
			it.State = "ignored"
		case queued[e.Path]:
			it.State = "queued"
		default:
			if rec, ok := done[e.Path]; ok {
				it.State = "done"
				it.Saved = rec.SrcSize - rec.DestSize
			} else {
				it.State = "skipped"
			}
		}
		counts[it.State]++
		if it.Codec != "" {
			codecs[it.Codec]++
		}

		if q != "" && !strings.Contains(strings.ToLower(e.Path), q) {
			continue
		}
		if wantState != "" && it.State != wantState {
			continue
		}
		if wantCodec != "" && it.Codec != wantCodec {
			continue
		}
		items = append(items, it)
	}

	switch r.URL.Query().Get("sort") {
	case "size":
		sort.Slice(items, func(i, j int) bool { return items[i].Size > items[j].Size })
	default:
		sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	}

	total := len(items)
	offset := intParam(r, "offset", 0, 0)
	limit := intParam(r, "limit", 60, 300)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := items[offset:end]

	// Looking for artwork means several stat() calls per file, which over NFS is
	// worth doing only for the page being sent.
	for i := range page {
		page[i].HasArt = FindArt(page[i].Path) != ""
	}

	writeJSON(w, map[string]any{
		"total": total, "items": page,
		"counts": counts, "codecs": codecs,
	})
}

// handleArt serves the artwork for a file, scaled down. Only paths in the index
// are accepted, so the interface cannot be used to read arbitrary images off
// the machine.
func (s *server) handleArt(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if _, ok := LoadIndex().Get(path); !ok {
		httpErr(w, http.StatusNotFound, "that file is not in the library index")
		return
	}
	src := FindArt(path)
	if src == "" {
		httpErr(w, http.StatusNotFound, "no artwork for this file")
		return
	}
	thumb, err := artThumb(r.Context(), src)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	// The thumbnail is derived from a file that rarely changes, and the cache key
	// already covers its size and timestamp, so it can be cached hard.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, thumb)
}

// --- doctor ---

// handleDoctorFile diagnoses a single file. The path has to be one vtrans knows
// about: taking any path from the request would let a caller probe arbitrary
// files on the machine through the interface.
func (s *server) handleDoctorFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpErr(w, http.StatusBadRequest, "no file given")
		return
	}

	var size int64
	known := false
	for _, c := range s.candidates() {
		if c.Path == path {
			size, known = c.Size, true
			break
		}
	}
	if !known {
		// Files that were processed or ignored are no longer candidates but are
		// still legitimate subjects; the index is the wider list.
		if e, ok := LoadIndex().Get(path); ok {
			size, known = e.Size, true
		}
	}
	if !known {
		httpErr(w, http.StatusNotFound, "that file is not in the library index")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	writeJSON(w, Diagnose(ctx, s.cfg, path, size))
}

func (s *server) handleDoctorScanGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.scan.snapshot())
}

func (s *server) handleDoctorScanStart(w http.ResponseWriter, r *http.Request) {
	// The whole index, not just the queue: a file that was processed months ago
	// or that the rules skip can still have something wrong with it, and nothing
	// else in vtrans would ever look.
	ix := LoadIndex()
	var targets []scanTarget
	for _, e := range ix.Entries {
		targets = append(targets, scanTarget{Path: e.Path, Size: e.Size})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Path < targets[j].Path })

	if len(targets) == 0 {
		httpErr(w, http.StatusBadRequest, "the index is empty, run 'vtrans scan' first")
		return
	}
	if !s.scan.start(s.cfg, targets) {
		httpErr(w, http.StatusConflict, "a scan is already running")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "total": len(targets)})
}

func (s *server) handleDoctorScanStop(w http.ResponseWriter, r *http.Request) {
	s.scan.stop()
	writeJSON(w, map[string]any{"ok": true})
}

// --- ignore list ---

func (s *server) handleIgnored(w http.ResponseWriter, r *http.Request) {
	recs := loadIgnoredRecords()
	sort.Slice(recs, func(i, j int) bool { return recs[i].At.After(recs[j].At) })

	// Sizes come from the index so the interface can say how much is being left
	// aside. A file that has since disappeared simply has no size.
	size := map[string]int64{}
	for _, c := range s.candidates() {
		size[c.Path] = c.Size
	}

	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, map[string]any{
			"path": rec.Src, "name": filepath.Base(rec.Src),
			"reason": rec.Reason, "auto": rec.Auto, "at": rec.At,
			"size": size[rec.Src],
		})
	}
	writeJSON(w, out)
}

// handleIgnore takes a file out of the queue for good.
func (s *server) handleIgnore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "could not parse the request: %v", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		httpErr(w, http.StatusBadRequest, "no file given")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "ignored by hand"
	}
	if err := Ignore(req.Path, reason, false); err != nil {
		httpErr(w, http.StatusInternalServerError, "could not write the ignore list: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": req.Path})
}

// handleUnignore puts a file back in the queue.
func (s *server) handleUnignore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		All  bool   `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "could not parse the request: %v", err)
		return
	}
	if req.All {
		if err := rewriteIgnored(nil); err != nil {
			httpErr(w, http.StatusInternalServerError, "could not clear the ignore list: %v", err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "removed": "all"})
		return
	}
	found, err := Unignore(req.Path)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "could not write the ignore list: %v", err)
		return
	}
	if !found {
		httpErr(w, http.StatusNotFound, "that file is not on the ignore list")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": req.Path})
}

// handleFailedClear removes a failure record so the file gets retried.
func (s *server) handleFailedClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		All  bool   `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "could not parse the request: %v", err)
		return
	}

	if req.All {
		if err := os.Remove(failedLogPath()); err != nil && !os.IsNotExist(err) {
			httpErr(w, http.StatusInternalServerError, "could not delete: %v", err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "removed": "all"})
		return
	}
	if req.Path == "" {
		httpErr(w, http.StatusBadRequest, "path is empty")
		return
	}

	recs := loadFailedRecords()
	var kept []FailRecord
	n := 0
	for _, rec := range recs {
		if rec.Src == req.Path {
			n++
			continue
		}
		kept = append(kept, rec)
	}
	if n == 0 {
		httpErr(w, http.StatusNotFound, "record not found")
		return
	}
	if err := rewriteFailed(kept); err != nil {
		httpErr(w, http.StatusInternalServerError, "could not write: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "removed": n})
}

// --- configuration ---

func (s *server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := LoadConfig()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"config": cfg, "path": configPath()})
}

func (s *server) handleConfigPost(w http.ResponseWriter, r *http.Request) {
	// Decoded over the defaults: fields the client omits fall back to the default
	// and an unknown field is ignored silently.
	cfg := DefaultConfig()
	if err := json.NewDecoder(r.Body).Decode(cfg); err != nil {
		httpErr(w, http.StatusBadRequest, "could not parse the request: %v", err)
		return
	}
	if err := validateConfig(cfg); err != nil {
		httpErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := SaveConfig(cfg); err != nil {
		httpErr(w, http.StatusInternalServerError, "could not save: %v", err)
		return
	}

	s.mu.Lock()
	s.cfg = cfg
	s.cands = nil // the candidate list depends on the configuration, rebuild it
	s.mu.Unlock()

	writeJSON(w, map[string]any{"ok": true, "config": cfg})
}

// validateConfig checks the values before saving.
//
// Values arriving from the interface are not trusted, and a bad one accepted
// silently here surfaces later in a service running at midnight: e.g. empty
// Roots makes the whole library invisible, and an invalid Mode would count as
// replace and overwrite originals.
func validateConfig(c *Config) error {
	if len(c.Roots) == 0 {
		return fmt.Errorf("at least one library root is required")
	}
	for _, r := range c.Roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("the library root must be an absolute path: %s", r)
		}
		if st, err := os.Stat(r); err != nil || !st.IsDir() {
			return fmt.Errorf("could not read the library root: %s", r)
		}
	}
	if c.Mode != ModeReplace && c.Mode != ModeCopy {
		return fmt.Errorf("mode must be 'replace' or 'copy'")
	}
	if c.Mode == ModeCopy && c.DestRoot == "" {
		return fmt.Errorf("the output root cannot be empty in copy mode")
	}
	if c.TrashDir != "" && !filepath.IsAbs(c.TrashDir) {
		return fmt.Errorf("the trash directory must be an absolute path")
	}
	for _, q := range []struct {
		name string
		v    int
	}{{"movie quality", c.QMovie}, {"tv quality", c.QTV},
		{"nvenc movie quality", c.QMovieNvenc}, {"nvenc tv quality", c.QTVNvenc}} {
		if q.v < 1 || q.v > 255 {
			return fmt.Errorf("%s must be between 1 and 255 (given: %d)", q.name, q.v)
		}
	}
	if c.TVMaxWidth < 0 || c.MovieMaxWidth < 0 {
		return fmt.Errorf("the width cap cannot be negative")
	}
	switch c.AudioMode {
	case "copy", "opus", "none":
	default:
		return fmt.Errorf("audio mode must be 'copy', 'opus' or 'none'")
	}
	if c.OpusStereoKbps < 8 || c.OpusStereoKbps > 512 {
		return fmt.Errorf("the opus bitrate must be between 8 and 512 kbps")
	}
	if c.MinBitrateMbps < 0 {
		return fmt.Errorf("the minimum bitrate cannot be negative")
	}
	if c.MinSavingRatio < 0 || c.MinSavingRatio >= 1 {
		return fmt.Errorf("the minimum saving ratio must be between 0 and 1")
	}
	switch c.Backend {
	case BackendAuto, BackendVAAPI, BackendNVENC:
	default:
		return fmt.Errorf("backend must be 'vaapi', 'nvenc' or empty for auto-detection")
	}
	switch c.ResolveBackend() {
	case BackendNVENC:
		if _, err := os.Stat(nvidiaControlDevice); err != nil {
			return fmt.Errorf("NVIDIA driver not loaded: %s not found", nvidiaControlDevice)
		}
		if c.CudaDevice == "" {
			return fmt.Errorf("the cuda device cannot be empty (use 0 for the first card)")
		}
		switch c.NvencPreset {
		case "", "p1", "p2", "p3", "p4", "p5", "p6", "p7":
		default:
			return fmt.Errorf("the nvenc preset must be p1..p7")
		}
	default:
		if c.RenderDevice == "" {
			return fmt.Errorf("the render device cannot be empty")
		}
		if _, err := os.Stat(c.RenderDevice); err != nil {
			return fmt.Errorf("render device not found: %s", c.RenderDevice)
		}
	}
	switch c.VerifyMode {
	case "sample", "full":
	default:
		return fmt.Errorf("verification mode must be 'sample' or 'full'")
	}
	if c.DurationToleranceSec < 0 {
		return fmt.Errorf("the duration tolerance cannot be negative")
	}
	if c.StableSeconds < 1 {
		return fmt.Errorf("the settle time must be at least 1 second")
	}
	if c.RescanMinutes < 0 {
		return fmt.Errorf("the rescan interval cannot be negative")
	}
	return nil
}

// --- trash ---

func (s *server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	entries, err := ListTrash(s.cfg)
	if err != nil {
		httpErr(w, http.StatusInternalServerError, "could not read the trash: %v", err)
		return
	}
	if entries == nil {
		entries = []TrashEntry{}
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	writeJSON(w, map[string]any{"dir": s.cfg.TrashDir, "entries": entries, "total": total})
}

// trashPathRequest reads the one field both single-file trash actions take.
func trashPathRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		httpErr(w, http.StatusBadRequest, "a trash file path is required")
		return "", false
	}
	return req.Path, true
}

// trashBusy reports whether a running job is working on the file a trash
// entry belongs to. The run lock cannot be used here: the service holds it
// for the whole batch, which under systemd means always, and a Restore that
// never works is no Restore. The only real conflict is the worker being
// mid-encode or mid-replace on that same original, and run.json says which
// file that is.
func trashBusy(cfg *Config, trashPath string) (string, bool) {
	st := LoadRunState()
	if !st.Alive() || st.Current == "" {
		return "", false
	}
	if trashPathFor(cfg, st.Current) == trashPath {
		return st.Current, true
	}
	return "", false
}

func (s *server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	p, ok := trashPathRequest(w, r)
	if !ok {
		return
	}
	if cur, busy := trashBusy(s.cfg, p); busy {
		httpErr(w, http.StatusConflict, "a run is working on %s right now; try again when it has moved on", filepath.Base(cur))
		return
	}

	orig, err := RestoreFromTrash(s.cfg, p)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "original": orig})
}

func (s *server) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := trashPathRequest(w, r)
	if !ok {
		return
	}
	if cur, busy := trashBusy(s.cfg, p); busy {
		httpErr(w, http.StatusConflict, "a run is working on %s right now; try again when it has moved on", filepath.Base(cur))
		return
	}

	if err := DeleteFromTrash(s.cfg, p); err != nil {
		httpErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) handleTrashEmpty(w http.ResponseWriter, r *http.Request) {
	if s.cfg.TrashDir == "" {
		httpErr(w, http.StatusBadRequest, "the trash directory is not configured")
		return
	}
	// A running job may be moving an original into that very directory; if the
	// lock cannot be taken, emptying is refused.
	release, err := acquireLock()
	if err != nil {
		httpErr(w, http.StatusConflict, "%v", err)
		return
	}
	defer release()

	ts := s.trashStat()
	if err := os.RemoveAll(s.cfg.TrashDir); err != nil {
		httpErr(w, http.StatusInternalServerError, "could not delete: %v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "files": ts.Files, "freed": ts.Size})
}
