package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	assets "github.com/AutisticShark/Subtitle-Translator"
	"github.com/AutisticShark/Subtitle-Translator/internal/app"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	healthcheck := flag.Bool("healthcheck", false, "check the running HTTP server")
	showVersion := flag.Bool("version", false, "print the server version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *healthcheck {
		return checkHealth(os.Getenv("LISTEN_ADDR"), os.Getenv("PORT"))
	}
	if _, err := fs.Stat(assets.Frontend(), "index.html"); err != nil {
		return errors.New("frontend is missing: run npm ci and npm run build in web/ before building the server")
	}
	config, err := app.ConfigFromEnv()
	if err != nil {
		return err
	}
	application, err := app.New(config, assets.Frontend(), assets.Locales())
	if err != nil {
		return err
	}
	defer application.Close()
	address := config.Addr
	if address == "" {
		address = "127.0.0.1:8000"
	}
	server := &http.Server{
		Addr: address, Handler: application.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() {
		slog.Info("Subtitle Translator is listening", "address", address, "version", version)
		result <- server.ListenAndServe()
	}()
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
	}
	return nil
}

func checkHealth(address, port string) error {
	if address == "" {
		if port == "" {
			port = "8000"
		}
		address = net.JoinHostPort("127.0.0.1", port)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("invalid healthcheck listen address")
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return errors.New("HTTP healthcheck failed")
	}
	defer response.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&body) != nil || body.Status != "ok" {
		return errors.New("HTTP healthcheck returned an unhealthy response")
	}
	return nil
}
