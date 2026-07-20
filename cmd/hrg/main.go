// Command hrg is the Home Runbook Generator: collectors observe your
// infrastructure, the store versions what they see, and the web UI (and
// eventually the runbook artifact) presents it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/breed007/hrg/internal/collector"
	"github.com/breed007/hrg/internal/collector/adguard"
	"github.com/breed007/hrg/internal/collector/docker"
	"github.com/breed007/hrg/internal/collector/manual"
	"github.com/breed007/hrg/internal/collector/netbox"
	"github.com/breed007/hrg/internal/collector/proxmox"
	"github.com/breed007/hrg/internal/collector/unifi"
	"github.com/breed007/hrg/internal/secrets"
	"github.com/breed007/hrg/internal/store"
	"github.com/breed007/hrg/internal/web"
)

// version is stamped at build time by GoReleaser (-X main.version=...).
var version = "dev"

func main() {
	var (
		dbPath      = flag.String("db", "hrg.db", "path to the SQLite database")
		addr        = flag.String("addr", "127.0.0.1:8080", "listen address (localhost by default; put a reverse proxy in front for anything wider)")
		resources   = flag.String("resources", "resources.d", "path to the manual resources.d directory")
		keyPath     = flag.String("key", "hrg.key", "path to the token-encryption key file (created on first use)")
		dev         = flag.Bool("dev", false, "enable developer affordances (e.g. the collector fixture_dir field — do not use in production)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `hrg — Home Runbook Generator

Usage:
  hrg [flags] serve     collect once, then serve the web UI (default)
  hrg [flags] collect   run all collectors once and exit

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("hrg", version)
		return
	}

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "serve"
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	collector.Register(proxmox.Type, proxmox.Factory)
	collector.Register(docker.Type, docker.Factory)
	collector.Register(unifi.Type, unifi.Factory)
	collector.Register(netbox.Type, netbox.Factory)
	collector.Register(adguard.Type, adguard.Factory)

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	key, err := secrets.LoadOrCreateKey(*keyPath)
	if err != nil {
		log.Error("load key", "err", err)
		os.Exit(1)
	}

	static := []collector.Collector{
		manual.New(*resources),
	}

	srv, err := web.NewServer(st, key, static, *resources, log, *dev)
	if err != nil {
		log.Error("init server", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()
	switch cmd {
	case "collect":
		srv.CollectAll(ctx)
	case "serve":
		srv.CollectAll(ctx)
		srv.StartScheduler(ctx)
		defer srv.StopScheduler()
		log.Info("serving", "addr", "http://"+*addr)
		if err := http.ListenAndServe(*addr, srv); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}
