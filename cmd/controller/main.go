package main

import (
	"flag"
	"log"
	"net/http"
	"path/filepath"

	"github.com/apeters/homebench/internal/controller"
	"github.com/apeters/homebench/web"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	configPath := flag.String("config", "homebench-config.json", "path to persistent JSON configuration")
	flag.Parse()

	cfgPath := *configPath
	if cfgPath != "" {
		if abs, err := filepath.Abs(cfgPath); err == nil {
			cfgPath = abs
		}
	}

	srv := controller.NewServer(web.FS, cfgPath)
	srv.StartBackground()

	log.Printf("homebench controller listening on %s (config %s)", *addr, cfgPath)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
