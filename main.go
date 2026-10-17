package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"freezen/internal/config"
	"freezen/internal/logging"
	"freezen/internal/server"
	"freezen/internal/zen"
)

// freezen serves OpenCode Zen's free models, in the three request formats the
// models are asked at: OpenAI Chat Completions, OpenAI Responses and Anthropic
// Messages. This is the entry point: it reads the config file, builds the
// provider and the server around it, and listens.

func main() {
	configPath := flag.String("c", "config.yaml", "path to the YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("freezen: config: %v", err)
	}
	logger := logging.New(cfg.LogLevel)

	provider, err := zen.New(cfg, logger)
	if err != nil {
		logger.Errorf("freezen: %v", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(provider, logger),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		provider.Discover(context.Background())
		for {
			time.Sleep(zen.ModelRefresh)
			provider.Discover(context.Background())
		}
	}()
	logger.Infof("freezen %s: %s serving %s on %s (proxy: %s)", zen.PluginVersion, zen.ProviderID, zen.Base, cfg.Addr, provider.ProxyName())
	if err := httpServer.ListenAndServe(); err != nil {
		logger.Errorf("freezen: %v", err)
		os.Exit(1)
	}
}
