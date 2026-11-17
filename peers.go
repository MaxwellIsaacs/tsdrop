package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

const lsPort = 53317

// Peer is a tailnet node, annotated with its LocalSend identity if it answered.
type Peer struct {
	Name      string `json:"name"` // MagicDNS short name (iPhones report HostName "localhost")
	HostName  string `json:"hostname"`
	OS        string `json:"os"`
	IP        string `json:"ip"`
	Online    bool   `json:"online"`
	LocalSend bool   `json:"localsend"`
	Alias     string `json:"alias,omitempty"`
	Protocol  string `json:"protocol,omitempty"` // https or http
	Web       bool   `json:"web,omitempty"`      // no LocalSend, but its browser can pull from the outbox
}

func (p Peer) Label() string {
	if p.Alias != "" && p.Alias != p.Name {
		return fmt.Sprintf("%s (%s)", p.Alias, p.Name)
	}
	return p.Name
}

type tsStatus struct {
	Peer map[string]struct {
		HostName     string
		DNSName      string
		OS           string
		Online       bool
		TailscaleIPs []string
	}
}

func tailnetPeers() ([]Peer, error) {
	out, err := exec.Command("tailscale", "status", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st tsStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	var peers []Peer
	for _, n := range st.Peer {
		if len(n.TailscaleIPs) == 0 {
			continue
		}
		name, _, _ := strings.Cut(n.DNSName, ".")
		if name == "" {
			name = n.HostName
		}
		peers = append(peers, Peer{Name: name, HostName: n.HostName, OS: n.OS, IP: n.TailscaleIPs[0], Online: n.Online})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	return peers, nil
}

// LocalSend apps use self-signed certs; the tailnet already authenticates peers.
// LocalSend >=1.17 also requires a client cert (mutual TLS).
var insecure = &http.Transport{TLSClientConfig: &tls.Config{
	InsecureSkipVerify: true,
	GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, _, err := identity()
		return &cert, err
	},
}}

func lsURL(p Peer, path string) string {
	proto := p.Protocol
	if proto == "" {
		proto = "https"
	}
	return fmt.Sprintf("%s://%s/api/localsend/v2/%s", proto, net.JoinHostPort(p.IP, fmt.Sprint(lsPort)), path)
}

// probe asks a peer for its LocalSend info, trying https then http.
func probe(ctx context.Context, p *Peer) {
	c := &http.Client{Transport: insecure}
	for _, proto := range []string{"https", "http"} {
		p.Protocol = proto
		req, _ := http.NewRequestWithContext(ctx, "GET", lsURL(*p, "info"), nil)
		resp, err := c.Do(req)
		if err != nil {
			continue
		}
		var info struct{ Alias string }
		json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			p.LocalSend, p.Alias = true, info.Alias
			return
		}
	}
	p.Protocol = ""
}

// discover lists tailnet peers and probes the online ones concurrently.
func discover(timeout time.Duration) ([]Peer, error) {
	peers, err := tailnetPeers()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := range peers {
		if !peers[i].Online {
			continue
		}
		wg.Add(1)
		go func(p *Peer) { defer wg.Done(); probe(ctx, p) }(&peers[i])
	}
	wg.Wait()
	for i := range peers {
		peers[i].Web = !peers[i].LocalSend && peers[i].OS == "iOS"
	}
	return peers, nil
}

func cmdPeers(args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	all := fs.Bool("all", false, "include offline peers")
	asJSON := fs.Bool("json", false, "JSON output")
	timeout := fs.Duration("timeout", 2*time.Second, "LocalSend probe timeout")
	fs.Parse(args)

	peers, err := discover(*timeout)
	if err != nil {
		return err
	}
	if !*all {
		var on []Peer
		for _, p := range peers {
			if p.Online {
				on = append(on, p)
			}
		}
		peers = on
	}
	if *asJSON {
		if peers == nil {
			peers = []Peer{}
		}
		return json.NewEncoder(os.Stdout).Encode(peers)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\tPEER\tOS\tIP\tSTATE")
	for _, p := range peers {
		mark, state := " ", "online"
		switch {
		case p.LocalSend:
			mark, state = "●", "localsend"
		case p.Web:
			mark, state = "◐", "outbox (tsdrop web app)"
		case !p.Online:
			state = "offline"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", mark, p.Label(), p.OS, p.IP, state)
	}
	return tw.Flush()
}
