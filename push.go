package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Web Push for the outbox: when something is queued, every subscribed web app
// (the iPhone home-screen PWA, served over HTTPS by `tailscale serve`) gets a
// notification that opens it on that item. Keys and subscriptions live next
// to the outbox: <state>/vapid.json, <state>/push-subs.json.

const pushTTL = 24 * 60 * 60 // seconds; matches how long a "something's waiting" ping stays useful

type pushSub struct {
	webpush.Subscription
	Origin string    `json:"origin"` // https origin it subscribed from; used as the VAPID subject
	Added  time.Time `json:"added"`
}

type pusher struct {
	dir       string
	pub, priv string
	mu        sync.Mutex
	subs      map[string]pushSub // by endpoint
	client    *http.Client
}

func newPusher(dir string) (*pusher, error) {
	p := &pusher{dir: dir, subs: map[string]pushSub{}, client: &http.Client{Timeout: 15 * time.Second}}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "vapid.json")
	var keys struct {
		Public  string `json:"public_key"`
		Private string `json:"private_key"`
	}
	b, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		keys.Private, keys.Public, err = webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		out, _ := json.MarshalIndent(keys, "", "  ")
		if err := os.WriteFile(keyPath, out, 0o600); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &keys); err != nil || keys.Public == "" || keys.Private == "" {
			return nil, fmt.Errorf("%s is malformed; delete it to regenerate (phones must then re-enable notifications)", keyPath)
		}
	}
	p.pub, p.priv = keys.Public, keys.Private
	if b, err := os.ReadFile(filepath.Join(dir, "push-subs.json")); err == nil {
		var list []pushSub
		json.Unmarshal(b, &list)
		for _, s := range list {
			p.subs[s.Endpoint] = s
		}
	}
	return p, nil
}

func (p *pusher) saveLocked() {
	list := make([]pushSub, 0, len(p.subs))
	for _, s := range p.subs {
		list = append(list, s)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := filepath.Join(p.dir, ".push-subs.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(p.dir, "push-subs.json"))
	}
}

func (p *pusher) handleKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"key": p.pub, "subscribed": len(p.subs)})
}

func (p *pusher) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	var s pushSub
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&s.Subscription); err != nil || s.Endpoint == "" || s.Keys.P256dh == "" || s.Keys.Auth == "" {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return
	}
	s.Origin, s.Added = "https://"+r.Host, time.Now()
	p.mu.Lock()
	_, known := p.subs[s.Endpoint]
	p.subs[s.Endpoint] = s
	p.saveLocked()
	p.mu.Unlock()
	if !known {
		log.Printf("push: new subscription (%d total)", len(p.subs))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *pusher) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var s webpush.Subscription
	json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&s)
	p.mu.Lock()
	delete(p.subs, s.Endpoint)
	p.saveLocked()
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (p *pusher) handleTest(w http.ResponseWriter, r *http.Request) {
	n := p.notify(map[string]string{"title": "tsdrop", "body": "notifications are on ✓", "url": "/"})
	fmt.Fprintf(w, "sent to %d\n", n)
}

// notifyItem announces a queued outbox item.
func (p *pusher) notifyItem(it boxItem) {
	body := it.Name
	if it.Text != "" {
		body = it.Text
		if r := []rune(body); len(r) > 160 {
			body = string(r[:160]) + "…"
		}
	}
	p.notify(map[string]string{"title": "tsdrop · from " + it.From, "body": body, "url": "/#" + it.ID, "tag": it.ID})
}

// notify sends to every subscription, pruning ones the push service says are gone.
func (p *pusher) notify(msg map[string]string) int {
	payload, _ := json.Marshal(msg)
	p.mu.Lock()
	subs := make([]pushSub, 0, len(p.subs))
	for _, s := range p.subs {
		subs = append(subs, s)
	}
	p.mu.Unlock()
	sent := 0
	for _, s := range subs {
		sub := s.Subscription
		resp, err := webpush.SendNotification(payload, &sub, &webpush.Options{
			HTTPClient:      p.client,
			Subscriber:      s.Origin,
			VAPIDPublicKey:  p.pub,
			VAPIDPrivateKey: p.priv,
			TTL:             pushTTL,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			log.Printf("push: %v", err)
			continue
		}
		resp.Body.Close()
		switch {
		case resp.StatusCode == 404 || resp.StatusCode == 410:
			p.mu.Lock()
			delete(p.subs, s.Endpoint)
			p.saveLocked()
			p.mu.Unlock()
			log.Printf("push: pruned expired subscription")
		case resp.StatusCode >= 300:
			log.Printf("push: %s", resp.Status)
		default:
			sent++
		}
	}
	return sent
}
