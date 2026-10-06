package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Waking a device on the console's home network.
//
// A sleeping PC cannot be reached over Tailscale: nothing on it is running.
// But the console sits on the same home network, and a Wake-on-LAN packet
// only has to come from there. So the status page, which can be opened from
// anywhere over the tailnet, gets a button that makes the console send one.

// wakeTarget is a device that can be woken.
type wakeTarget struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

// parseMAC accepts the usual ways of writing a hardware address and returns
// it in the form aa:bb:cc:dd:ee:ff.
func parseMAC(s string) (net.HardwareAddr, error) {
	s = strings.TrimSpace(s)
	if len(s) == 12 && !strings.ContainsAny(s, ":-.") {
		s = s[0:2] + ":" + s[2:4] + ":" + s[4:6] + ":" + s[6:8] + ":" + s[8:10] + ":" + s[10:12]
	}
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("%q is not a network card address (it looks like 00:11:22:AA:BB:CC)", s)
	}
	return mac, nil
}

func validateWake(list []wakeTarget) error {
	for i := range list {
		list[i].Name = strings.TrimSpace(list[i].Name)
		mac, err := parseMAC(list[i].MAC)
		if err != nil {
			return err
		}
		list[i].MAC = mac.String()
		if list[i].Name == "" {
			list[i].Name = list[i].MAC
		}
		if len(list[i].Name) > 64 {
			return errors.New("a name is too long")
		}
	}
	return nil
}

// magicPacket is the Wake-on-LAN payload: six bytes of 0xff and the address
// sixteen times.
func magicPacket(mac net.HardwareAddr) []byte {
	p := make([]byte, 0, 6+16*6)
	for i := 0; i < 6; i++ {
		p = append(p, 0xff)
	}
	for i := 0; i < 16; i++ {
		p = append(p, mac...)
	}
	return p
}

// broadcastAddrs returns where to send a packet so that every device on the
// console's networks sees it: each network's own broadcast address, and the
// general one.
func broadcastAddrs() []net.IP {
	list := []net.IP{net.IPv4bcast}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return list
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() || len(ipnet.Mask) != 4 {
			continue
		}
		if ones, _ := ipnet.Mask.Size(); ones >= 31 {
			continue
		}
		b := make(net.IP, 4)
		for i := range b {
			b[i] = ip[i] | ^ipnet.Mask[i]
		}
		list = append(list, b)
	}
	return list
}

// sendWake broadcasts the magic packet. It reports how many sends went out;
// there is no way to know whether the device heard it.
func sendWake(mac net.HardwareAddr) (sent int, err error) {
	packet := magicPacket(mac)
	var lastErr error
	for _, ip := range broadcastAddrs() {
		// Port 9 is the customary one; some network cards listen on 7.
		for _, port := range []int{9, 7} {
			c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: port})
			if err != nil {
				lastErr = err
				continue
			}
			if _, err := c.Write(packet); err != nil {
				lastErr = err
			} else {
				sent++
			}
			c.Close()
		}
	}
	if sent == 0 {
		if lastErr == nil {
			lastErr = errors.New("no network to send on")
		}
		return 0, lastErr
	}
	return sent, nil
}

// handleWakeList replaces the list of devices that can be woken.
func (d *daemon) handleWakeList(w http.ResponseWriter, r *http.Request) {
	var list []wakeTarget
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&list); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateWake(list); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.cfg.Wake = list
	cfg := d.cfg
	d.mu.Unlock()
	if err := saveConfig(d.cfgPath, cfg); err != nil {
		d.logf("saving config: %v", err)
	}
	io.WriteString(w, "ok\n")
}

// handleWake sends the wake-up packet to one of the listed devices. Only
// listed devices: the page is not a tool for poking arbitrary addresses.
func (d *daemon) handleWake(w http.ResponseWriter, r *http.Request) {
	mac, err := parseMAC(r.URL.Query().Get("mac"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	name := ""
	for _, t := range d.cfg.Wake {
		if t.MAC == mac.String() {
			name = t.Name
		}
	}
	d.mu.Unlock()
	if name == "" {
		http.Error(w, "that device is not in the list; save it first", http.StatusNotFound)
		return
	}
	sent, err := sendWake(mac)
	if err != nil {
		d.logf("wake %s: %v", name, err)
		http.Error(w, "could not send the wake-up packet: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d.logf("wake-up packet sent to %s (%d sends)", name, sent)
	io.WriteString(w, "Wake-up packet sent to "+name+". Give it half a minute.\n")
}
