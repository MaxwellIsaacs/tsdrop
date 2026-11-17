package main

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
)

// The web drop is plain HTTP: WireGuard already encrypts the tailnet, and iOS
// Shortcuts/Safari won't trust a self-signed cert.
const webPort = lsPort + 1

//go:embed web.html
var webHTML string

//go:embed assets/tsdrop.svg
var iconSVG []byte

// iOS home-screen icons must be PNG.
//
//go:embed assets/tsdrop-180.png
var iconPNG []byte

//go:embed assets/tsdrop-192.png
var icon192 []byte

//go:embed assets/tsdrop-512.png
var icon512 []byte

//go:embed sw.js
var swJS []byte

//go:embed manifest.webmanifest
var manifest []byte

func (rv *receiver) webHandler() http.Handler {
	page := template.Must(template.New("page").Parse(webHTML))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		alias, dir := rv.info.Alias, rv.dir
		if rv.forward != "" {
			alias, dir = rv.forward, rv.forward+" (held on "+rv.info.Alias+" until it's online)"
		}
		page.Execute(w, map[string]string{"Alias": alias, "Dir": dir, "HTTPS": rv.httpsURL})
	})
	mux.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(iconSVG)
	})
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(iconPNG)
	})
	for path, png := range map[string][]byte{"/icon-192.png": icon192, "/icon-512.png": icon512} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(png)
		})
	}
	mux.HandleFunc("POST /drop", rv.handleDrop)
	if up := rv.outboxUpstream(); up != nil {
		// This box sends to someone else's outbox (the relay); show that one here too.
		px := httputil.NewSingleHostReverseProxy(up)
		mux.Handle("/outbox", px)
		mux.Handle("/outbox/", px)
		mux.Handle("/push/", px)
	} else {
		mux.HandleFunc("GET /outbox", rv.handleBoxList)
		mux.HandleFunc("POST /outbox", rv.handleBoxPost)
		mux.HandleFunc("GET /outbox/{id}", rv.handleBoxFile)
		mux.HandleFunc("DELETE /outbox/{id}", rv.handleBoxDelete)
		if rv.push != nil {
			mux.HandleFunc("GET /push/key", rv.push.handleKey)
			mux.HandleFunc("POST /push/subscribe", rv.push.handleSubscribe)
			mux.HandleFunc("POST /push/unsubscribe", rv.push.handleUnsubscribe)
			mux.HandleFunc("POST /push/test", rv.push.handleTest)
		}
	}
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(swJS)
	})
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(manifest)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fromTailnet(r) {
			http.Error(w, "tailnet only", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// handleDrop takes multipart uploads (the page), a raw body with ?name= (Shortcuts
// "File" body), or text (?text= or a text/plain body without a name).
func (rv *receiver) handleDrop(w http.ResponseWriter, r *http.Request) {
	sender := r.URL.Query().Get("from")
	if sender == "" {
		sender = "web"
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var saved []string

	switch {
	case ct == "multipart/form-data":
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if part.FileName() == "" {
				if part.FormName() == "text" {
					b, _ := io.ReadAll(io.LimitReader(part, 1<<20))
					rv.gotText(sender, string(b))
				}
				continue
			}
			name := path.Base(part.FileName())
			if path.Ext(name) == "" {
				pct, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
				name += extFor(pct)
			}
			dest, err := rv.save(fileMeta{ID: randID(), FileName: name}, part)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			saved = append(saved, dest)
		}
	case ct == "application/x-www-form-urlencoded" && r.URL.Query().Get("name") == "":
		// Shortcuts sends a Form with only Text fields this way: text=...
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rv.gotText(sender, r.PostForm.Get("text"))
	case r.URL.Query().Get("text") != "":
		rv.gotText(sender, r.URL.Query().Get("text"))
	case r.URL.Query().Get("name") == "" && (ct == "text/plain" || ct == "application/x-www-form-urlencoded" || ct == ""):
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rv.gotText(sender, string(b))
	default:
		name := path.Base(r.URL.Query().Get("name"))
		if name == "." || name == "/" {
			name = "drop-" + randID()
		}
		if path.Ext(name) == "" {
			name += extFor(ct)
		}
		dest, err := rv.save(fileMeta{ID: randID(), FileName: name}, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		saved = append(saved, dest)
	}

	for _, s := range saved {
		log.Printf("web: saved %s", s)
	}
	if len(saved) > 0 {
		rv.wake()
		rv.announce(sender, saved)
	}
	fmt.Fprintf(w, "ok %d\n", len(saved))
}

func (rv *receiver) gotText(sender, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	log.Printf("text from %s: %q", sender, text)
	if rv.forward != "" {
		rv.queueText(text)
		return
	}
	copyClipboard(text)
	notify("tsdrop · "+sender, text+"\n(copied to clipboard)")
}

func extFor(ct string) string {
	switch ct { // mime's first pick for these is obscure (.jfif, .mov is fine)
	case "image/jpeg":
		return ".jpg"
	case "image/heic":
		return ".heic"
	}
	if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// outboxUpstream is the configured outbox when it lives on another host, so a
// phone pointed at this box still sees what was sent to it.
func (rv *receiver) outboxUpstream() *url.URL {
	if rv.forward != "" {
		return nil // the relay owns its outbox
	}
	raw, err := outboxURL()
	if err != nil {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Hostname() == rv.webHost || u.Hostname() == "127.0.0.1" {
		return nil
	}
	return u
}
