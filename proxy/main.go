// gpu-wake-proxy: a wake-on-demand reverse proxy for a GPU machine that suspends when idle.
//
// It runs on a small always-on machine on the same LAN. Each app on the GPU host gets its own
// loopback listener. When the host does not answer, the proxy sends a Wake-on-LAN magic packet,
// waits until the app is healthy and forwards the request. Browsers get a waiting page in the
// meantime. Responses stream through without buffering.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	cfgPath := flag.String("config", "/etc/gpu-wake/proxy.json", "config file")
	check := flag.Bool("check", false, "validate the config and exit")
	wakeNow := flag.Bool("wake", false, "send one magic packet and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps

	if *showVersion {
		fmt.Println("gpu-wake-proxy", version)
		return
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	pages, err := loadPages(cfg.Pages)
	if err != nil {
		log.Fatal(err)
	}
	if *check {
		fmt.Println("config ok")
		return
	}
	if *wakeNow {
		if err := sendMagic(cfg.Host); err != nil {
			log.Fatal(err)
		}
		fmt.Println("magic packet sent to", cfg.Host.MAC)
		return
	}
	log.Fatal(run(context.Background(), cfg, pages))
}

func run(ctx context.Context, cfg *Config, pages *Pages) error {
	log.Printf("gpu-wake-proxy %s", version)
	m := newMachine(cfg)
	lim := newLimiter(cfg.WakesPerIPHour)
	br := loadBridge(cfg.StateFile, cfg.PublicWakesPerHour)
	errc := make(chan error, len(cfg.Upstreams)+1)
	var ups []*Upstream
	for _, uc := range cfg.Upstreams {
		u, err := newUpstream(uc, cfg, m, lim, br, pages)
		if err != nil {
			return err
		}
		ups = append(ups, u)
		log.Printf("%s: %s -> %s (%s, wake %s, max %d in flight)",
			uc.Name, uc.Listen, uc.Target, uc.Audience, uc.WakePolicy, uc.MaxInflight)
		go serve(uc.Listen, u, errc)
	}
	if cfg.AdminListen != "" {
		a := &admin{m: m, ups: ups, bridge: br, pages: pages, start: time.Now()}
		log.Printf("admin: %s", cfg.AdminListen)
		go serve(cfg.AdminListen, a.routes(), errc)
	}
	go m.Watch(ctx, 30*time.Second)
	return <-errc
}

func serve(addr string, h http.Handler, errc chan<- error) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc <- fmt.Errorf("%s: %w", addr, srv.ListenAndServe())
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: gpu-wake-proxy [-config FILE] [-check | -wake | -version]\n")
		flag.PrintDefaults()
	}
}
