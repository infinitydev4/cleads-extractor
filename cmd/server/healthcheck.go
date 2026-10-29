package main

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// healthcheck probes the local HTTP listener and returns the process exit
// code. It is the Docker HEALTHCHECK of the distroless image, which ships no
// shell, curl or wget: "cleads-extractor healthcheck [url]".
func healthcheck(args []string, getenv func(string) string, stderr io.Writer) int {
	port := getenv("PORT")
	if port == "" {
		port = "8080"
	}
	url := "http://127.0.0.1:" + port + "/healthz"
	if len(args) > 0 {
		url = args[0]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "healthcheck: unexpected status", resp.Status)
		return 1
	}
	return 0
}
