package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The outbox is the other direction of the web drop: a browser page can't be
// listened to, so files for the phone wait here until the page fetches them.
// Arch (or anything on the tailnet) POSTs to /outbox; the PWA polls GET /outbox.

const outboxTTL = 7 * 24 * time.Hour // policy: un-fetched items are pruned after a week

type boxItem struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Type string    `json:"type"`
	Text string    `json:"text,omitempty"` // set for text messages; no file then
	From string    `json:"from"`
	Time time.Time `json:"time"`
}

func outboxDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tsdrop", "outbox")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tsdrop", "outbox")
}

// Each item is <id>.json (metadata) plus <id> (body) for files.
func (rv *receiver) boxList() []boxItem {
	ents, _ := os.ReadDir(rv.outbox)
	var items []boxItem
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(rv.outbox, e.Name()))
		var it boxItem
		if err != nil || json.Unmarshal(b, &it) != nil {
			continue
		}
		if time.Since(it.Time) > outboxTTL {
			rv.boxDelete(it.ID)
			continue
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Time.After(items[j].Time) })
	return items
}

func (rv *receiver) boxGet(id string) (boxItem, error) {
	var it boxItem
	if id != path.Base(id) || strings.HasPrefix(id, ".") {
		return it, errors.New("bad id")
	}
	b, err := os.ReadFile(filepath.Join(rv.outbox, id+".json"))
	if err != nil {
		return it, err
	}
	return it, json.Unmarshal(b, &it)
}

func (rv *receiver) boxDelete(id string) {
	if id != path.Base(id) || strings.HasPrefix(id, ".") {
		return
	}
	os.Remove(filepath.Join(rv.outbox, id))
	os.Remove(filepath.Join(rv.outbox, id+".json"))
}

func (rv *receiver) boxPut(it boxItem, body io.Reader) error {
	if err := os.MkdirAll(rv.outbox, 0o700); err != nil {
		return err
	}
	if body != nil {
		tmp := filepath.Join(rv.outbox, ".tmp-"+it.ID)
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, body)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			return err
		}
		it.Size = n
		if err := os.Rename(tmp, filepath.Join(rv.outbox, it.ID)); err != nil {
			return err
		}
	}
	meta, _ := json.Marshal(it)
	tmp := filepath.Join(rv.outbox, ".tmp-"+it.ID+".json")
	if err := os.WriteFile(tmp, meta, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(rv.outbox, it.ID+".json"))
}

// POST /outbox?name=F&from=H  raw body = file;  ?text= or a nameless text body = message.
func (rv *receiver) handleBoxPost(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	it := boxItem{ID: time.Now().UTC().Format("20060102T150405") + "-" + randID(), From: q.Get("from"), Time: time.Now()}
	if it.From == "" {
		it.From = "tailnet"
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var err error
	switch name := path.Base(q.Get("name")); {
	case q.Get("text") != "":
		it.Text, it.Type = q.Get("text"), "text/plain"
	case q.Get("name") == "":
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		it.Text, it.Type = strings.TrimSpace(string(b)), "text/plain"
	default:
		it.Name = name
		it.Type = mimeOf(name)
		if it.Type == "application/octet-stream" && ct != "" && ct != "application/octet-stream" {
			it.Type = ct
		}
		err = rv.boxPut(it, r.Body)
	}
	if it.Text != "" {
		err = rv.boxPut(it, nil)
	} else if it.Name == "" {
		http.Error(w, "empty", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("outbox: queued %s from %s", firstNonEmpty(it.Name, "text"), it.From)
	if rv.push != nil {
		go rv.push.notifyItem(it)
	}
	fmt.Fprintln(w, "ok", it.ID)
}

func (rv *receiver) handleBoxList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	items := rv.boxList()
	if items == nil {
		items = []boxItem{}
	}
	json.NewEncoder(w).Encode(items)
}

func (rv *receiver) handleBoxFile(w http.ResponseWriter, r *http.Request) {
	it, err := rv.boxGet(r.PathValue("id"))
	if err != nil || it.Name == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(rv.outbox, it.ID))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	disp := "inline"
	if r.URL.Query().Has("dl") {
		disp = "attachment"
	}
	w.Header().Set("Content-Type", it.Type)
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disp, url.PathEscape(it.Name)))
	http.ServeContent(w, r, it.Name, it.Time, f)
}

func (rv *receiver) handleBoxDelete(w http.ResponseWriter, r *http.Request) {
	rv.boxDelete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// --- sending side ---------------------------------------------------------

// outboxURL is where `tsdrop send` parks things for devices without a listener
// (the iPhone PWA). TSDROP_OUTBOX, else ~/.config/tsdrop/outbox (one URL).
func outboxURL() (string, error) {
	if u := os.Getenv("TSDROP_OUTBOX"); u != "" {
		return strings.TrimRight(u, "/"), nil
	}
	cfg, _ := os.UserConfigDir()
	b, err := os.ReadFile(filepath.Join(cfg, "tsdrop", "outbox"))
	if err != nil {
		return "", errors.New("no outbox configured (put the relay's web URL in ~/.config/tsdrop/outbox)")
	}
	return strings.TrimRight(strings.TrimSpace(string(b)), "/"), nil
}

func postOutbox(items []outgoing, pr *progress) error {
	base, err := outboxURL()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	for _, it := range items {
		pr.total += it.meta.Size
	}
	if !pr.jsonOut && !pr.quiet {
		fmt.Fprintf(os.Stderr, "→ outbox %s: %d item(s), %s\n", base, len(items), human(pr.total))
	}
	c := &http.Client{}
	for _, it := range items {
		q := url.Values{"from": {host}}
		var body io.Reader
		if it.path == "" {
			body = strings.NewReader(string(it.body)) // text: nameless body
		} else {
			f, err := os.Open(it.path)
			if err != nil {
				return err
			}
			defer f.Close()
			body = f
			q.Set("name", path.Base(it.meta.FileName))
		}
		req, _ := http.NewRequest("POST", base+"/outbox?"+q.Encode(), &countingReader{r: body, p: pr, name: it.meta.FileName})
		req.ContentLength = it.meta.Size
		req.Header.Set("Content-Type", it.meta.FileType)
		resp, err := c.Do(req)
		if err != nil {
			return fmt.Errorf("outbox: %w", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("outbox: %s %s", resp.Status, strings.TrimSpace(string(b)))
		}
	}
	pr.event("done", map[string]any{})
	if !pr.jsonOut && !pr.quiet {
		fmt.Fprintln(os.Stderr, "\n✓ waiting in the outbox — open tsdrop on the phone")
	}
	return nil
}
