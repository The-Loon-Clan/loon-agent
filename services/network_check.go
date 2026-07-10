package services

import (
	"io"
	"log"
	"net/http"
	"time"
	"github.com/ameNZB/usenet-pipeline/config"
	"github.com/ameNZB/usenet-pipeline/storage"
)

func MonitorNetworkConnection(cfg *config.Config) {
	var lastStatus string
	for {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get("https://api.ipify.org")

		storage.GlobalState.Lock()
		if err != nil {
			if lastStatus != "Disconnected" {
				log.Printf("Network check failed! VPN might be down: %v", err)
				lastStatus = "Disconnected"
			}
			storage.GlobalState.VPNStatus = "Disconnected"
			storage.GlobalState.PublicIP = "Unknown"
		} else {
			ip, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil {
				ipStr := string(ip)
				if lastStatus != "Connected" {
					log.Printf("VPN Connected. Current Public IP: %s", ipStr)
					lastStatus = "Connected"
				}
				storage.GlobalState.VPNStatus = "Connected"
				storage.GlobalState.PublicIP = ipStr
			}
		}
		storage.GlobalState.Unlock()
		time.Sleep(60 * time.Second)
	}
}
