// Command healthcheck is a minimal static HTTP probe for distroless images.
//
// The distroless runtime images used by Dockerfile.api and Dockerfile.worker
// ship no shell, so classic shell-based HEALTHCHECK commands (curl/wget) are
// impossible there. This tiny Go binary is compiled alongside the services and
// copied into the images; docker-compose.prod.yml runs it as the container
// healthcheck:
//
//	/app/healthcheck http://127.0.0.1:8080/readyz
//
// It exits 0 when the target answers with a 2xx status inside the timeout and
// 1 otherwise. Failures print a one-line reason so
// `docker inspect --format '{{json .State.Health}}'` stays useful.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// probeTimeout bounds the whole request, matching a compose healthcheck
// timeout of a few seconds with headroom.
const probeTimeout = 3 * time.Second

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: healthcheck <url>")
		os.Exit(1)
	}
	target := os.Args[1]
	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck %s: %v\n", target, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck %s: status %s\n", target, resp.Status)
		os.Exit(1)
	}
}
