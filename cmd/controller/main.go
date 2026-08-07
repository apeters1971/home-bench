package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/apeters/homebench/internal/controller"
	"github.com/apeters/homebench/web"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	srv := controller.NewServer(web.FS)
	srv.StartBackground()

	log.Printf("homebench controller listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
