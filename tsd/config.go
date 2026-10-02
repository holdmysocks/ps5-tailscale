package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
)

// config is read from /data/tailscale/config.json. Every field is optional.
type config struct {
	// Hostname is the name this console gets on the tailnet.
	Hostname string `json:"hostname"`
	// AuthKey, if set, logs the console in without the browser step.
	AuthKey string `json:"authKey,omitempty"`
	// WebAddr is where the status page listens.
	WebAddr string `json:"webAddr"`
	// HTTPProxyAddr is where the outbound HTTP proxy listens. Pointing the
	// PS5's proxy setting at it lets the console reach tailnet hosts. Empty
	// disables the proxy.
	HTTPProxyAddr string `json:"httpProxyAddr"`
	// ControlURL selects a coordination server other than Tailscale's.
	ControlURL string `json:"controlURL,omitempty"`
	// SunshineHost is a tailnet device running Sunshine. When set, its
	// streaming ports are forwarded from 127.0.0.1, so a Moonlight client on
	// the console can use 127.0.0.1 as the host.
	SunshineHost string `json:"sunshineHost,omitempty"`
	// Forwards are extra local forwards: a localhost port on the console
	// relayed to a host on the tailnet.
	Forwards []forwardRule `json:"forwards,omitempty"`
	// UDPPorts lists the console's UDP ports that are reachable from the
	// tailnet. The default is what PS5 Remote Play uses. An empty list turns
	// inbound UDP off.
	UDPPorts []uint16 `json:"udpPorts"`
	// BlockedPorts lists local TCP ports that are never exposed to the tailnet.
	BlockedPorts []uint16 `json:"blockedPorts,omitempty"`
	// Verbose turns on Tailscale's own (very chatty) logging.
	Verbose bool `json:"verbose,omitempty"`
}

func defaultConfig() config {
	return config{
		Hostname:      "ps5",
		WebAddr:       ":8090",
		HTTPProxyAddr: "127.0.0.1:8118",
		UDPPorts:      slices.Clone(remotePlayUDPPorts),
	}
}

// loadConfig reads the config file, writing one with the defaults if it does
// not exist yet so that there is something to edit.
func loadConfig(path string) (config, error) {
	cfg := defaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, saveConfig(path, cfg)
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return defaultConfig(), err
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "ps5"
	}
	if cfg.WebAddr == "" {
		cfg.WebAddr = ":8090"
	}
	return cfg, nil
}

func saveConfig(path string, cfg config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
