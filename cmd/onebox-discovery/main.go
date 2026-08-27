package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/onebox/internal/discovery"
)

func main() {
	var socket, output, network, application string
	flag.StringVar(&socket, "socket", "/var/run/docker.sock", "Docker Engine Unix socket")
	flag.StringVar(&output, "output", "/dynamic/onebox.yml", "atomic Traefik dynamic configuration output")
	flag.StringVar(&network, "network", "ob-ingress", "Docker network carrying routed backends")
	flag.StringVar(&application, "app", "", "Onebox Compose project to observe")
	flag.Parse()
	if application == "" {
		fmt.Fprintln(os.Stderr, "onebox-discovery: --app is required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := discovery.NewDockerClient(socket)
	reconcile := func() error {
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		containers, err := client.Containers(requestCtx, application)
		if err != nil {
			return err
		}
		document, err := discovery.Build(containers, network)
		if err != nil {
			return err
		}
		return discovery.WriteAtomic(output, document)
	}

	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		// Subscribe from just before observation. Docker replays matching
		// events, closing the reconcile→subscribe race without making event
		// ordering part of correctness; duplicate rebuilds are harmless.
		since := time.Now().Add(-time.Second)
		if err := reconcile(); err != nil {
			log.Printf("reconcile: %v", err)
		} else {
			backoff = 250 * time.Millisecond
		}
		err := client.Events(ctx, application, since, reconcile)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			log.Printf("events: %v", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}
