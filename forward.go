package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store-and-forward: with --forward, an always-on box takes drops for a machine
// that sleeps. Files sit in the spool until the peer answers, then are removed.

const textSpool = ".text" // queued text messages, one file each

func (rv *receiver) wake() {
	select {
	case rv.kick <- struct{}{}:
	default:
	}
}

func (rv *receiver) queueText(text string) {
	dir := filepath.Join(rv.dir, textSpool)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("forward: %v", err)
		return
	}
	// Timestamp prefix keeps delivery in arrival order.
	name := time.Now().UTC().Format("20060102T150405.000000000") + "-" + randID()
	tmp := filepath.Join(dir, ".tmp-"+name)
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		log.Printf("forward: %v", err)
		return
	}
	os.Rename(tmp, filepath.Join(dir, name))
	rv.wake()
}

func (rv *receiver) forwardLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		rv.flush()
		select {
		case <-rv.kick:
		case <-t.C:
		}
	}
}

// pending lists spooled items, skipping in-flight temp files (dot-prefixed).
func (rv *receiver) pending() (texts, files []string) {
	filepath.WalkDir(rv.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == rv.dir {
			return nil
		}
		hidden := strings.HasPrefix(d.Name(), ".")
		if d.IsDir() {
			if hidden && d.Name() != textSpool {
				return filepath.SkipDir
			}
			return nil
		}
		if hidden {
			return nil
		}
		if filepath.Base(filepath.Dir(p)) == textSpool && filepath.Dir(filepath.Dir(p)) == rv.dir {
			texts = append(texts, p)
		} else {
			files = append(files, p)
		}
		return nil
	})
	return
}

func (rv *receiver) flush() {
	texts, files := rv.pending()
	if len(texts)+len(files) == 0 {
		return
	}
	peer, err := pick(rv.forward)
	if err != nil || !peer.LocalSend {
		return // asleep or off the tailnet; try again next tick
	}
	for _, p := range texts {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		it := outgoing{body: b, meta: fileMeta{ID: randID(), FileName: randID() + ".txt", Size: int64(len(b)), FileType: "text/plain", Preview: string(b)}}
		if err := deliver(peer, []outgoing{it}, &progress{quiet: true}); err != nil {
			log.Printf("forward text → %s: %v", peer.Label(), err)
			return
		}
		os.Remove(p)
		log.Printf("forwarded text → %s", peer.Label())
	}
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(rv.dir, p)
		it := outgoing{path: p, meta: fileMeta{ID: randID(), FileName: filepath.ToSlash(rel), Size: st.Size(), FileType: mimeOf(p)}}
		if err := deliver(peer, []outgoing{it}, &progress{quiet: true}); err != nil {
			log.Printf("forward %s → %s: %v", rel, peer.Label(), err)
			return
		}
		os.Remove(p)
		// Drop now-empty sub-folders the sender created.
		for d := filepath.Dir(p); d != rv.dir; d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
		log.Printf("forwarded %s → %s", rel, peer.Label())
	}
}
