package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/apeters/homebench/internal/client"
)

func main() {
	controller := flag.String("controller", "http://127.0.0.1:8080", "controller base URL")
	hostname := flag.String("hostname", "", "override hostname (default: os hostname)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agent := client.NewAgent(*controller, *hostname)
	log.Printf("homebench client starting — controller=%s hostname=%s", *controller, agent.Hostname)
	if err := agent.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
