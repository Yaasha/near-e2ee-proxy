package main

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// healthy checks process liveness only. Do not load credentials, attest models,
// follow redirects or use ambient proxies for a periodic local probe.
func healthy() bool {
	listen := os.Getenv("LISTEN_ADDR")
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return false
	}
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
		if ip != nil && ip.To4() == nil {
			host = "::1"
		}
	} else if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}
