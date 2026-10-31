// Command animind-seed is a local torrent seeder for end-to-end verification.
//
// It builds a metainfo from a directory of small files, seeds it from
// 127.0.0.1 with DHT, PEX and trackers disabled — there is no public swarm and
// nothing leaves the loopback — and prints the magnet (with the seeder's own
// x.pe address, BEP 9) and info hash so a verify script can drive a real
// download job against it without any tracker infrastructure.
//
// Usage:
//
//	animind-seed -dir ./fixture-content [-port 6881] [-json]
//
// With -json the only stdout output is one line:
//
//	{"magnet":"magnet:?xt=urn:btih:...","infoHash":"..."}
//
// The process keeps running (seeding) until it receives SIGINT/SIGTERM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// verifyGrace bounds the initial hash check. The fixture is a few hundred
// kilobytes; anything longer means something is wrong with the storage root.
const verifyGrace = 2 * time.Minute

func main() {
	dir := flag.String("dir", "", "directory of small files to seed (required)")
	port := flag.Int("port", 0, "TCP/UDP listen port on 127.0.0.1 (0 picks a free port)")
	asJSON := flag.Bool("json", false, "print one {\"magnet\": ..., \"infoHash\": ...} JSON line instead of the human-readable form")
	flag.Parse()

	if *dir == "" {
		fatal(errors.New("-dir is required"))
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fatal(fmt.Errorf("resolve -dir: %w", err))
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		fatal(fmt.Errorf("-dir %s is not a directory", abs))
	}

	// Build the metainfo from the directory tree: Name is the base name, so the
	// directory name is the torrent title the API's release parser will see.
	var info metainfo.Info
	if err := info.BuildFromFilePath(abs); err != nil {
		fatal(fmt.Errorf("build metainfo from %s: %w", abs, err))
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		fatal(fmt.Errorf("encode metainfo: %w", err))
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	ih := mi.HashInfoBytes()

	// Deterministic local-only swarm: no DHT, no PEX, no trackers, no port
	// mapping. Peers find the seeder through the magnet's x.pe parameter alone.
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoDHT = true
	cfg.DisablePEX = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.Seed = true
	// The literal loopback host is IPv4-only; without this upstream also opens
	// a tcp6 listener for the same address and fails to start on hosts without
	// IPv6 ("subsequent listen: listen tcp6").
	cfg.DisableIPv6 = true
	cfg.SetListenAddr(net.JoinHostPort("127.0.0.1", fmt.Sprint(*port)))
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		fatal(fmt.Errorf("torrent client: %w", err))
	}
	defer cl.Close()

	// The storage root is the PARENT of the seeded directory: the info's Name
	// is the directory's base, so file paths resolve back onto the real files.
	spec := &torrent.TorrentSpec{
		AddTorrentOpts: torrent.AddTorrentOpts{
			InfoHash:  ih,
			InfoBytes: infoBytes,
			Storage:   storage.NewFile(filepath.Dir(abs)),
		},
		DisplayName: info.BestName(),
	}
	t, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		fatal(fmt.Errorf("add torrent: %w", err))
	}

	// Hash-check the on-disk data before announcing readiness, so the download
	// side never waits on pieces the seeder has but has not verified.
	verifyCtx, cancel := context.WithTimeout(context.Background(), verifyGrace)
	defer cancel()
	if err := t.VerifyDataContext(verifyCtx); err != nil {
		fatal(fmt.Errorf("verify seeded data: %w", err))
	}

	listen := localListenAddr(cl)
	m := metainfo.Magnet{InfoHash: ih, DisplayName: info.BestName()}
	if listen != "" {
		m.Params = url.Values{"x.pe": []string{listen}}
	}
	payload := struct {
		Magnet   string `json:"magnet"`
		InfoHash string `json:"infoHash"`
	}{Magnet: m.String(), InfoHash: ih.HexString()}

	if *asJSON {
		line, err := json.Marshal(payload)
		if err != nil {
			fatal(fmt.Errorf("encode payload: %w", err))
		}
		fmt.Println(string(line))
	} else {
		fmt.Printf("magnet=%s\ninfoHash=%s\nlistening=%s\n", payload.Magnet, payload.InfoHash, listen)
	}

	// Seed until interrupted. The swarm never completes a job without the
	// seeder alive, so exiting early is the one way this helper can fail a
	// verification silently.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

// localListenAddr returns the client's 127.0.0.1 listen address with the
// resolved port (the -port value or whatever the kernel picked for 0).
func localListenAddr(cl *torrent.Client) string {
	for _, addr := range cl.ListenAddrs() {
		host, port, err := net.SplitHostPort(addr.String())
		if err == nil && (host == "127.0.0.1" || host == "::1") {
			return net.JoinHostPort("127.0.0.1", port)
		}
	}
	return ""
}

// fatal reports startup failures on stderr and exits nonzero; nothing on stdout
// may precede the payload line, which is what the verify script parses.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "animind-seed:", err)
	os.Exit(1)
}
