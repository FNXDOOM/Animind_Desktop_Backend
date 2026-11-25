// Command animind-seed is a local torrent seeder for end-to-end verification. It builds a
// metainfo from a directory, seeds it from 127.0.0.1 with DHT, PEX and trackers disabled, then
// prints the magnet (carrying x.pe) and info hash until SIGINT or SIGTERM.
//
//	animind-seed -dir ./fixture-content [-port 6881] [-json]
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

// verifyGrace bounds the initial hash check; a longer wait means the storage root is wrong.
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

	// Name is the directory's base name, so it becomes the torrent title the release parser sees.
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

	// Local-only swarm: no DHT, PEX, trackers or port mapping; peers find us via the magnet's x.pe.
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoDHT = true
	cfg.DisablePEX = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.Seed = true
	// The literal loopback host is IPv4-only; without this upstream also opens a tcp6 listener
	// for the same address and fails on hosts without IPv6.
	cfg.DisableIPv6 = true
	cfg.SetListenAddr(net.JoinHostPort("127.0.0.1", fmt.Sprint(*port)))
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		fatal(fmt.Errorf("torrent client: %w", err))
	}
	defer cl.Close()

	// The storage root is the parent: the info's Name is the directory's base name.
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

	// Verify on-disk data before announcing readiness so downloads never wait on unverified pieces.
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

	// Seed until interrupted; exiting early is the one way this helper fails a verification
	// silently.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

// localListenAddr returns the 127.0.0.1 listen address with the resolved port.
func localListenAddr(cl *torrent.Client) string {
	for _, addr := range cl.ListenAddrs() {
		host, port, err := net.SplitHostPort(addr.String())
		if err == nil && (host == "127.0.0.1" || host == "::1") {
			return net.JoinHostPort("127.0.0.1", port)
		}
	}
	return ""
}

// fatal reports startup failures on stderr; nothing on stdout may precede the payload line.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "animind-seed:", err)
	os.Exit(1)
}
