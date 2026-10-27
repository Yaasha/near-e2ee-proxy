// Command near-e2ee-proxy serves verified, encrypted NEAR Chat Completions through
// a local OpenAI-compatible endpoint. The local host is part of the trust boundary.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"near-e2ee-proxy/internal/near"
	"near-e2ee-proxy/internal/proxy"
)

func main() {
	generate := flag.Bool("generate-key", false, "print a fresh 256-bit proxy bearer token and exit")
	health := flag.Bool("healthcheck", false, "check local /healthz within two seconds and exit without credentials")
	flag.Parse()
	if flag.NArg() != 0 || (*generate && *health) {
		log.Fatal("unexpected arguments; use -help")
	}
	if *health {
		if !healthy() {
			os.Exit(1)
		}
		return
	}
	if *generate {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			log.Fatal("cannot generate a key")
		}
		fmt.Println(hex.EncodeToString(key[:]))
		return
	}
	config, err := proxy.LoadConfig()
	if err != nil {
		log.Fatal(err)
	} // Configuration errors are fixed messages, never values.
	client, err := near.New(config.NearKey)
	if err != nil {
		log.Fatal("cannot initialize confidential inference")
	}
	defer client.Close()
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		log.Fatal("cannot listen on LISTEN_ADDR")
	}
	server := &http.Server{
		Handler: proxy.NewServer(config, client), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: proxy.RequestTimeout + 10*time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			_ = server.Close()
		}
	}()
	log.Print("NEAR E2EE proxy listening; NEAR deployment controls are trusted")
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Print("HTTP service stopped unexpectedly")
		_ = client.Close()
		os.Exit(1)
	}
	// Serve returns as soon as Shutdown closes the listener. Wait for its active
	// requests to finish before closing the SDK and cancelling their verification.
	<-shutdownDone
}
