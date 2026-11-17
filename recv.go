package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tailscale's CGNAT and ULA ranges; recv only talks to tailnet peers.
var tailnets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48", "127.0.0.0/8"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func fromTailnet(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	for _, n := range tailnets {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

type session struct {
	id     string
	sender string
	files  map[string]fileMeta
	tokens map[string]string
	done   map[string]bool
	saved  []string
}

type receiver struct {
	dir  string
	info deviceInfo
	mu   sync.Mutex
	sess *session

	outbox   string        // items waiting for the web app to fetch
	push     *pusher       // nil when this box proxies the outbox elsewhere
	httpsURL string        // where `tailscale serve` exposes the web app over HTTPS (for push)
	webHost  string        // our own listen host, to spot a self-pointing outbox URL
	forward  string        // peer to relay everything to; dir is then just a spool
	kick     chan struct{} // wakes the forwarder after a drop
}

func cmdRecv(args []string) error {
	fs := flag.NewFlagSet("recv", flag.ExitOnError)
	home, _ := os.UserHomeDir()
	dir := fs.String("dir", filepath.Join(home, "Downloads"), "where received files go")
	addr := fs.String("addr", "", "listen address (default: this node's tailnet IPv4)")
	forward := fs.String("forward", "", "relay everything to this peer (store-and-forward; --dir becomes the spool)")
	outbox := fs.String("outbox", outboxDir(), "where items for the web app wait")
	httpsURL := fs.String("https-url", "", "HTTPS address of the web app (e.g. from tailscale serve), shown as a hint on the plain-HTTP page")
	fs.Parse(args)

	if *addr == "" {
		ip, err := waitTailnetIP()
		if err != nil {
			return err
		}
		*addr = net.JoinHostPort(ip, fmt.Sprint(lsPort))
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	cert, _, err := identity()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	rv := &receiver{dir: *dir, info: selfInfo(), forward: *forward, outbox: *outbox, httpsURL: *httpsURL, kick: make(chan struct{}, 1)}
	if rv.forward != "" {
		go rv.forwardLoop()
	}
	rv.webHost = host
	if rv.outboxUpstream() == nil {
		if rv.push, err = newPusher(filepath.Dir(rv.outbox)); err != nil {
			return fmt.Errorf("push: %w", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/localsend/v2/info", rv.handleInfo)
	mux.HandleFunc("/api/localsend/v2/register", rv.handleInfo)
	mux.HandleFunc("/api/localsend/v2/prepare-upload", rv.handlePrepare)
	mux.HandleFunc("/api/localsend/v2/upload", rv.handleUpload)
	mux.HandleFunc("/api/localsend/v2/cancel", rv.handleCancel)
	srv := &http.Server{
		Addr: *addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !fromTailnet(r) {
				http.Error(w, "tailnet only", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(w, r)
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequestClientCert},
		ReadHeaderTimeout: 10 * time.Second,
	}
	web := &http.Server{Addr: net.JoinHostPort(host, fmt.Sprint(webPort)), Handler: rv.webHandler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 3)
	go func() { errc <- srv.ListenAndServeTLS("", "") }()
	go func() { errc <- web.ListenAndServe() }()
	if rv.httpsURL != "" {
		// tailscale serve only proxies to loopback.
		lo := &http.Server{Addr: net.JoinHostPort("127.0.0.1", fmt.Sprint(webPort)), Handler: web.Handler, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- lo.ListenAndServe() }()
	}
	log.Printf("tsdrop recv: %s as %q → %s; web drop on http://%s", *addr, rv.info.Alias, *dir, web.Addr)
	return <-errc
}

// waitTailnetIP blocks until tailscaled hands out an address (boot races).
func waitTailnetIP() (string, error) {
	for i := 0; ; i++ {
		out, err := exec.Command("tailscale", "ip", "-4").Output()
		if ip := strings.TrimSpace(string(out)); err == nil && ip != "" {
			return ip, nil
		}
		if i == 60 {
			return "", errors.New("no tailnet IP after 5 minutes — is tailscale up?")
		}
		time.Sleep(5 * time.Second)
	}
}

func (rv *receiver) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rv.info)
}

func (rv *receiver) handlePrepare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Info  deviceInfo          `json:"info"`
		Files map[string]fileMeta `json:"files"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Files) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sender := req.Info.Alias
	if sender == "" {
		sender = r.RemoteAddr
	}

	// A lone text item with a preview is a message: no upload follows.
	if len(req.Files) == 1 {
		for _, f := range req.Files {
			if f.FileType == "text/plain" && f.Preview != "" {
				rv.gotText(sender, f.Preview)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}

	rv.mu.Lock()
	defer rv.mu.Unlock()
	s := &session{id: randID(), sender: sender, files: req.Files, tokens: map[string]string{}, done: map[string]bool{}}
	for id := range req.Files {
		s.tokens[id] = randID()
	}
	rv.sess = s // a newer request replaces a stalled one rather than wedging us
	log.Printf("session %s from %s: %d file(s)", s.id, sender, len(req.Files))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"sessionId": s.id, "files": s.tokens})
}

func (rv *receiver) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rv.mu.Lock()
	s := rv.sess
	var meta fileMeta
	ok := s != nil && s.id == q.Get("sessionId") && s.tokens[q.Get("fileId")] == q.Get("token") && q.Get("token") != ""
	if ok {
		meta = s.files[q.Get("fileId")]
	}
	rv.mu.Unlock()
	if !ok || r.Method != http.MethodPost {
		http.Error(w, "invalid session or token", http.StatusForbidden)
		return
	}

	dest, err := rv.save(meta, r.Body)
	if err != nil {
		log.Printf("upload %s: %v", meta.FileName, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	rv.mu.Lock()
	s.done[meta.ID] = true
	s.saved = append(s.saved, dest)
	finished := len(s.done) == len(s.files)
	if finished && rv.sess == s {
		rv.sess = nil
	}
	rv.mu.Unlock()
	log.Printf("saved %s", dest)
	rv.wake()
	if finished {
		rv.announce(s.sender, s.saved)
	}
}

func (rv *receiver) announce(sender string, saved []string) {
	body := filepath.Base(saved[0])
	if len(saved) > 1 {
		body = fmt.Sprintf("%s + %d more", body, len(saved)-1)
	}
	notify("tsdrop · "+sender, body+"\n→ "+rv.dir)
}

// save writes into dir, keeping sub-folders from the sender but never escaping dir.
func (rv *receiver) save(meta fileMeta, body io.Reader) (string, error) {
	rel := filepath.Clean("/" + filepath.FromSlash(meta.FileName))[1:]
	if rel == "" || rel == "." {
		rel = meta.ID
	}
	dest := uniquePath(filepath.Join(rv.dir, rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".tsdrop-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if meta.Size > 0 && n != meta.Size {
		return "", fmt.Errorf("short upload: got %d of %d bytes", n, meta.Size)
	}
	return dest, os.Rename(tmp.Name(), dest)
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

func (rv *receiver) handleCancel(w http.ResponseWriter, r *http.Request) {
	rv.mu.Lock()
	if rv.sess != nil && rv.sess.id == r.URL.Query().Get("sessionId") {
		rv.sess = nil
	}
	rv.mu.Unlock()
}

func notify(title, body string) {
	exec.Command("notify-send", "-a", "tsdrop", "-i", "tsdrop", title, body).Run()
}

func copyClipboard(text string) {
	cmd := exec.Command("wl-copy")
	cmd.Stdin = strings.NewReader(text)
	cmd.Run()
}
