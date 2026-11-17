package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type deviceInfo struct {
	Alias       string `json:"alias"`
	Version     string `json:"version"`
	DeviceModel string `json:"deviceModel"`
	DeviceType  string `json:"deviceType"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
	Protocol    string `json:"protocol"`
	Download    bool   `json:"download"`
}

type fileMeta struct {
	ID       string `json:"id"`
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
	FileType string `json:"fileType"`
	Preview  string `json:"preview,omitempty"`
}

type outgoing struct {
	meta fileMeta
	path string // empty for text messages
	body []byte // text payload
}

func selfInfo() deviceInfo {
	host, _ := os.Hostname()
	_, fp, _ := identity()
	return deviceInfo{
		Alias: host, Version: "2.1", DeviceModel: "tsdrop", DeviceType: "desktop",
		Fingerprint: fp, Port: lsPort, Protocol: "https",
	}
}

func randID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func mimeOf(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	return "application/octet-stream"
}

// collect expands args into files; directories keep their relative layout.
func collect(paths []string) ([]outgoing, error) {
	var out []outgoing
	for _, root := range paths {
		st, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			out = append(out, outgoing{path: root, meta: fileMeta{ID: randID(), FileName: filepath.Base(root), Size: st.Size(), FileType: mimeOf(root)}})
			continue
		}
		base := filepath.Dir(filepath.Clean(root))
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, p)
			out = append(out, outgoing{path: p, meta: fileMeta{ID: randID(), FileName: filepath.ToSlash(rel), Size: info.Size(), FileType: mimeOf(p)}})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pick resolves --to, or prompts among LocalSend-ready peers.
func pick(to string) (Peer, error) {
	peers, err := discover(2 * time.Second)
	if err != nil {
		return Peer{}, err
	}
	var ready []Peer
	for _, p := range peers {
		if p.LocalSend {
			ready = append(ready, p)
		}
	}
	if to != "" {
		q := strings.ToLower(to)
		var hits []Peer
		for _, p := range peers {
			if p.IP == to {
				hits = []Peer{p}
				break
			}
			if strings.Contains(strings.ToLower(p.Name+" "+p.HostName+" "+p.Alias), q) {
				hits = append(hits, p)
			}
		}
		if len(hits) == 0 && net.ParseIP(to) != nil {
			p := Peer{Name: to, IP: to, Online: true}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			probe(ctx, &p)
			cancel()
			hits = []Peer{p}
		}
		switch {
		case len(hits) == 0:
			return Peer{}, fmt.Errorf("no tailnet peer matches %q", to)
		case len(hits) > 1:
			var names []string
			for _, h := range hits {
				names = append(names, h.Label())
			}
			return Peer{}, fmt.Errorf("%q is ambiguous: %s", to, strings.Join(names, ", "))
		case !hits[0].LocalSend && !hits[0].Web:
			return Peer{}, fmt.Errorf("%s isn't answering on :%d — is LocalSend open on it?", hits[0].Label(), lsPort)
		}
		return hits[0], nil
	}
	switch len(ready) {
	case 0:
		return Peer{}, errors.New("no tailnet peer is running LocalSend — use --to PHONE to leave it in the outbox")
	case 1:
		return ready[0], nil
	}
	for i, p := range ready {
		fmt.Fprintf(os.Stderr, "  %d) %s  %s  %s\n", i+1, p.Label(), p.OS, p.IP)
	}
	fmt.Fprint(os.Stderr, "send to which? ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(ready) {
		return Peer{}, errors.New("no peer chosen")
	}
	return ready[n-1], nil
}

// progress reports either human-readable lines on stderr or JSON events on stdout.
type progress struct {
	jsonOut bool
	quiet   bool // no output at all (the forwarder)
	total   int64
	sent    int64
	last    time.Time
}

func (p *progress) event(kind string, kv map[string]any) {
	if !p.jsonOut {
		return
	}
	kv["event"] = kind
	json.NewEncoder(os.Stdout).Encode(kv)
}

func (p *progress) add(n int64, name string) {
	p.sent += n
	if time.Since(p.last) < 100*time.Millisecond && p.sent < p.total {
		return
	}
	p.last = time.Now()
	if p.quiet {
		return
	}
	if p.jsonOut {
		p.event("progress", map[string]any{"sent": p.sent, "total": p.total, "file": name})
		return
	}
	pct := 100.0
	if p.total > 0 {
		pct = float64(p.sent) * 100 / float64(p.total)
	}
	fmt.Fprintf(os.Stderr, "\r  %5.1f%%  %s / %s  %s\033[K", pct, human(p.sent), human(p.total), name)
}

type countingReader struct {
	r    io.Reader
	p    *progress
	name string
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.p.add(int64(n), c.name)
	return n, err
}

func human(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 {
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1fTB", f)
}

func cmdSend(args []string) error {
	fset := flag.NewFlagSet("send", flag.ExitOnError)
	to := fset.String("to", "", "peer name, alias or IP")
	text := fset.String("text", "", "send a text message instead of files")
	jsonOut := fset.Bool("json", false, "emit JSON progress events on stdout (for the GUI)")
	fset.Parse(args)

	var items []outgoing
	if *text != "" {
		b := []byte(*text)
		items = append(items, outgoing{body: b, meta: fileMeta{ID: randID(), FileName: randID() + ".txt", Size: int64(len(b)), FileType: "text/plain", Preview: *text}})
	}
	files, err := collect(fset.Args())
	if err != nil {
		return err
	}
	items = append(items, files...)
	if len(items) == 0 {
		return errors.New("nothing to send: give files or --text")
	}

	peer, err := pick(*to)
	if err != nil {
		return err
	}
	pr := &progress{jsonOut: *jsonOut}
	if !peer.LocalSend {
		return postOutbox(items, pr)
	}
	return deliver(peer, items, pr)
}

// deliver runs one LocalSend session: prepare-upload, then each file.
func deliver(peer Peer, items []outgoing, pr *progress) error {
	for _, it := range items {
		pr.total += it.meta.Size
	}
	if !pr.jsonOut && !pr.quiet {
		fmt.Fprintf(os.Stderr, "→ %s: %d item(s), %s — waiting for them to accept…\n", peer.Label(), len(items), human(pr.total))
	}
	pr.event("waiting", map[string]any{"peer": peer.Label()})

	// prepare-upload blocks until the receiver taps accept.
	metas := map[string]fileMeta{}
	for _, it := range items {
		metas[it.meta.ID] = it.meta
	}
	reqBody, _ := json.Marshal(map[string]any{"info": selfInfo(), "files": metas})
	c := &http.Client{Transport: insecure, Timeout: 5 * time.Minute}
	resp, err := c.Post(lsURL(peer, "prepare-upload"), "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200:
	case 204:
		pr.event("done", map[string]any{})
		if !pr.jsonOut && !pr.quiet {
			fmt.Fprintln(os.Stderr, "✓ delivered")
		}
		return nil
	case 403:
		return errors.New("declined by receiver")
	case 401:
		return errors.New("receiver requires a PIN (turn it off in LocalSend settings)")
	case 409:
		return errors.New("receiver is busy with another transfer")
	default:
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("prepare-upload: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var sess struct {
		SessionID string            `json:"sessionId"`
		Files     map[string]string `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		return fmt.Errorf("prepare-upload response: %w", err)
	}

	up := &http.Client{Transport: insecure} // no overall timeout: large files take a while
	for _, it := range items {
		token, ok := sess.Files[it.meta.ID]
		if !ok {
			continue // receiver skipped this one
		}
		var body io.Reader = bytes.NewReader(it.body)
		if it.path != "" {
			f, err := os.Open(it.path)
			if err != nil {
				return err
			}
			defer f.Close()
			body = f
		}
		url := fmt.Sprintf("%s?sessionId=%s&fileId=%s&token=%s", lsURL(peer, "upload"), sess.SessionID, it.meta.ID, token)
		req, _ := http.NewRequest("POST", url, &countingReader{r: body, p: pr, name: it.meta.FileName})
		req.ContentLength = it.meta.Size
		req.Header.Set("Content-Type", "application/octet-stream")
		r, err := up.Do(req)
		if err != nil {
			up.Post(lsURL(peer, "cancel")+"?sessionId="+sess.SessionID, "", nil)
			return fmt.Errorf("upload %s: %w", it.meta.FileName, err)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("upload %s: %s", it.meta.FileName, r.Status)
		}
	}
	pr.event("done", map[string]any{})
	if !pr.jsonOut && !pr.quiet {
		fmt.Fprintln(os.Stderr, "\n✓ sent")
	}
	return nil
}
