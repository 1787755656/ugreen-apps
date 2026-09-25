package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	port := flag.Int("port", 21710, "management http port")
	dataDir := flag.String("data-dir", "", "data dir (default $UGAPP_DATA_DIR or ./data)")
	flag.Parse()

	dd := *dataDir
	if dd == "" {
		dd = os.Getenv("UGAPP_DATA_DIR")
	}
	if dd == "" {
		dd = "./data"
	}
	if err := os.MkdirAll(dd, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("executable path: %v", err)
	}
	exeDir := filepath.Dir(exe)

	// The p2pee-proxy binary ships next to the launcher inside bin/.
	proxyPath := filepath.Join(exeDir, "p2pee-proxy")
	if _, err := os.Stat(proxyPath); err != nil {
		// Local-dev fallback: built binary lives elsewhere.
		alt := filepath.Join(exeDir, "..", "rootfs_amd64", "bin", "p2pee-proxy")
		if _, e := os.Stat(alt); e == nil {
			proxyPath = alt
		}
	}

	// Static UI lives at <installdir>/www (bin/../www). Also try a couple of
	// dev layouts so `go run` works without the package installed.
	wwwPath := ""
	for _, cand := range []string{
		filepath.Join(exeDir, "..", "www"),
		filepath.Join(exeDir, "..", "..", "rootfs_common", "www"),
		filepath.Join(exeDir, "www"),
	} {
		if fi, e := os.Stat(cand); e == nil && fi.IsDir() {
			wwwPath = cand
			break
		}
	}

	mgr := NewManager(proxyPath, dd)
	mgr.LoadConfig()

	srv := &Server{mgr: mgr, www: wwwPath, session: NewSessionStore()}

	// Persist the running state across app restarts.
	if mgr.cfg.Running && mgr.cfg.Key != "" {
		if err := mgr.Start(); err != nil {
			log.Printf("auto-start skipped: %v", err)
		}
	}

	httpSrv := &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", *port), Handler: srv.Routes()}
	go func() {
		log.Printf("p2pee-proxy manager listening on :%d", *port)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down, stopping proxy if running")
	mgr.Stop()
	_ = httpSrv.Close()
}
