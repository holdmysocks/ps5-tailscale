package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
)

// config is read from /data/tailscale/config.json. Every field is optional.
// Most of it can be edited on the status page.
type config struct {
	// Hostname is the name this console gets on the tailnet.
	Hostname string `json:"hostname"`
	// AuthKey, if set, logs the console in without the browser step.
	AuthKey string `json:"authKey,omitempty"`
	// WebAddr is where the status page listens.
	WebAddr string `json:"webAddr"`
	// PasswordHash protects the status page. Empty means no password. It is
	// set from the status page; delete the field to remove a forgotten
	// password.
	PasswordHash string `json:"passwordHash,omitempty"`
	// HTTPProxyAddr is where the outbound HTTP proxy listens. Empty, the
	// default, turns the proxy off.
	HTTPProxyAddr string `json:"httpProxyAddr"`
	// ControlURL selects a coordination server other than Tailscale's.
	ControlURL string `json:"controlURL,omitempty"`
	// SunshineHosts are tailnet devices running Sunshine. Their streaming
	// ports are forwarded from 127.0.0.1, so a Moonlight client on the
	// console can use 127.0.0.1 as the host.
	SunshineHosts []sunshineHost `json:"sunshineHosts,omitempty"`
	// SunshineHost is the single-host setting of earlier versions. It is
	// folded into SunshineHosts when the config is loaded.
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
	// AllowFrom limits which tailnet devices may reach the console's services:
	// empty for every device the tailnet's access rules allow, "own" for only
	// the devices of the user this console is logged in as.
	AllowFrom string `json:"allowFrom,omitempty"`
	// PayloadPath names the copy of the payload that is started at boot, for
	// example in a payload manager's folder. An update installed from the
	// status page replaces that file too. Empty means there is none to keep
	// up to date.
	PayloadPath string `json:"payloadPath,omitempty"`
	// ReceiveDir is where files sent to the console with Taildrop end up.
	ReceiveDir string `json:"receiveDir"`
	// Priority is how the daemon competes for CPU time: "low" (the default)
	// never takes time from a game, "high" shares the CPU with games on
	// equal terms, which can make Remote Play smoother. Applied at start.
	Priority string `json:"priority,omitempty"`
	// CheckUpdates makes the daemon ask GitHub now and then whether a newer
	// release exists, to say so on the status page.
	CheckUpdates bool `json:"checkUpdates"`
	// Verbose turns on Tailscale's own (very chatty) logging.
	Verbose bool `json:"verbose,omitempty"`
}

func defaultConfig() config {
	return config{
		Hostname:     "ps5",
		WebAddr:      ":8090",
		ReceiveDir:   defaultReceiveDir,
		UDPPorts:     slices.Clone(remotePlayUDPPorts),
		CheckUpdates: true,
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
	cfg.normalize()
	return cfg, nil
}

// normalize fills in what must not be empty and brings settings from earlier
// versions into their current form.
func (cfg *config) normalize() {
	if cfg.Hostname == "" {
		cfg.Hostname = "ps5"
	}
	if cfg.WebAddr == "" {
		cfg.WebAddr = ":8090"
	}
	if cfg.SunshineHost != "" {
		known := slices.ContainsFunc(cfg.SunshineHosts, func(h sunshineHost) bool { return h.Host == cfg.SunshineHost })
		if !known {
			cfg.SunshineHosts = append(cfg.SunshineHosts, sunshineHost{Host: cfg.SunshineHost})
		}
		cfg.SunshineHost = ""
	}
	if cfg.ReceiveDir == "" {
		cfg.ReceiveDir = defaultReceiveDir
	}
	if cfg.AllowFrom != accessOwn {
		cfg.AllowFrom = ""
	}
	if cfg.Priority != priorityHigh {
		cfg.Priority = ""
	}
}

func saveConfig(path string, cfg config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
