package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RestartVPN asks the gluetun sidecar to drop its VPN tunnel and reconnect.
// Because the agent shares gluetun's network namespace (network_mode:
// "service:vpn" in docker-compose.yml), the control server is reachable at
// 127.0.0.1:8000 by default — no extra ports, no docker socket, no special
// privileges. Override with GLUETUN_CONTROL_URL if the topology differs.
//
// Gluetun doesn't expose a single "restart" verb, so we PUT status=stopped,
// wait briefly, then PUT status=running. The VPN_TYPE the user configured
// in docker-compose decides which endpoint to hit; we try both so the
// helper works for both OpenVPN and WireGuard setups without extra config.
func RestartVPN(ctx context.Context) error {
	base := os.Getenv("GLUETUN_CONTROL_URL")
	if base == "" {
		base = "http://127.0.0.1:8000"
	}

	// Short timeout — if gluetun's control server is wedged, we'd rather
	// fall through to the hard-exit path than block the watchdog loop.
	httpc := &http.Client{Timeout: 10 * time.Second}

	put := func(path, body string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+path, bytes.NewBufferString(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("gluetun %s returned %d: %s", path, resp.StatusCode, string(b))
		}
		return nil
	}

	// Try OpenVPN first (the docker-compose default), then WireGuard.
	// 404s on the wrong endpoint are expected and non-fatal.
	paths := []string{"/v1/openvpn/status", "/v1/wireguard/status"}
	var lastErr error
	for _, p := range paths {
		if err := put(p, `{"status":"stopped"}`); err != nil {
			lastErr = err
			continue
		}
		// Small gap so gluetun finishes tearing down before we ask it
		// to come back up.
		time.Sleep(2 * time.Second)
		if err := put(p, `{"status":"running"}`); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("all gluetun endpoints failed (last: %w)", lastErr)
}
