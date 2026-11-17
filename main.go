// tsdrop: pick a Tailscale peer and send it files over the LocalSend v2
// protocol; `recv` stands in for the LocalSend app on this end.
package main

import (
	"fmt"
	"os"
)

const usage = `tsdrop — LocalSend over Tailscale

usage:
  tsdrop peers [--all] [--json]                 list tailnet peers, marking ones running LocalSend
  tsdrop send [--to PEER] FILE|DIR...           send files (prompts for a peer if --to omitted)
  tsdrop send [--to PEER] --text "msg"          send a text message / link
                                                (an iPhone without LocalSend open gets it via the outbox → tsdrop web app)
  tsdrop gui                                    open the little picker window
  tsdrop recv [--dir DIR] [--addr ADDR]         receive from tailnet peers (LocalSend-compatible)
  tsdrop recv --forward PEER [--dir SPOOL]      always-on relay: hold drops until PEER is online, then pass them on
       [--https-url URL]                        where tailscale serve exposes the web app (needed for phone notifications)

PEER is a hostname, MagicDNS name, LocalSend alias, or tailnet IP (substring match).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "peers", "ls":
		err = cmdPeers(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "recv", "receive":
		err = cmdRecv(os.Args[2:])
	case "gui":
		err = cmdGUI(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tsdrop:", err)
		os.Exit(1)
	}
}
