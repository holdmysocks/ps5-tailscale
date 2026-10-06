package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func testPicker(t *testing.T, candidates []string, answering map[string]bool) (*dnsPicker, *[]string, *[]string) {
	var logs, probed []string
	p := &dnsPicker{
		logf:       func(format string, args ...any) { logs = append(logs, format) },
		candidates: func() []string { return candidates },
		probe: func(ctx context.Context, server string) bool {
			probed = append(probed, server)
			return answering[server]
		},
	}
	return p, &logs, &probed
}

func TestDNSPicker(t *testing.T) {
	candidates := []string{"127.0.0.1:53", "192.168.1.1:53", "1.1.1.1:53"}
	answering := map[string]bool{"127.0.0.1:53": true, "192.168.1.1:53": true, "1.1.1.1:53": true}
	p, logs, probed := testPicker(t, candidates, answering)
	ctx := context.Background()

	// The console's own DNS is preferred, and the choice is kept.
	if got := p.pick(ctx); got != "127.0.0.1:53" {
		t.Fatalf("picked %s", got)
	}
	p.pick(ctx)
	if len(*probed) != 1 {
		t.Errorf("probed %v; the choice should have been kept", *probed)
	}

	// No DNS payload: the router is used, and that is logged.
	answering["127.0.0.1:53"] = false
	p.reset()
	if got := p.pick(ctx); got != "192.168.1.1:53" {
		t.Errorf("without a local DNS: picked %s", got)
	}
	if last := (*logs)[len(*logs)-1]; !strings.Contains(last, "instead") {
		t.Errorf("log: %q", last)
	}

	// Nor the router: a public resolver.
	answering["192.168.1.1:53"] = false
	p.reset()
	if got := p.pick(ctx); got != "1.1.1.1:53" {
		t.Errorf("without local DNS or router: picked %s", got)
	}

	// Nothing at all: stay with the first, and look again soon.
	answering["1.1.1.1:53"] = false
	p.reset()
	if got := p.pick(ctx); got != "127.0.0.1:53" {
		t.Errorf("with nothing answering: picked %s", got)
	}
	if p.working {
		t.Error("the picker thinks it has a working server")
	}
	before := len(*probed)
	p.checked = time.Now().Add(-dnsRecheckBad - time.Second)
	answering["127.0.0.1:53"] = true
	if got := p.pick(ctx); got != "127.0.0.1:53" || len(*probed) == before || !p.working {
		t.Errorf("after the short wait: picked %s, working %v", got, p.working)
	}
}

func TestDNSQueryAndReply(t *testing.T) {
	q := dnsQuery(0x1234, "github.com")
	want := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 6, 'g', 'i', 't', 'h', 'u', 'b', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	if string(q) != string(want) {
		t.Fatalf("query = % x", q)
	}
	reply := func(id0, id1, flags2, flags3 byte, answers byte) []byte {
		return []byte{id0, id1, flags2, flags3, 0, 1, 0, answers, 0, 0, 0, 0}
	}
	for name, tt := range map[string]struct {
		r    []byte
		want bool
	}{
		"good answer":        {reply(0x12, 0x34, 0x81, 0x80, 2), true},
		"another query's id": {reply(0x12, 0x35, 0x81, 0x80, 2), false},
		"server failure":     {reply(0x12, 0x34, 0x81, 0x82, 0), false},
		"no such name":       {reply(0x12, 0x34, 0x81, 0x83, 0), false},
		"no answers":         {reply(0x12, 0x34, 0x81, 0x80, 0), false},
		"not a response":     {reply(0x12, 0x34, 0x01, 0x00, 1), false},
		"too short":          {[]byte{0x12, 0x34}, false},
	} {
		if got := dnsReplyOK(q, tt.r); got != tt.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

// A real exchange over UDP with a stand-in server, and a port where nothing
// answers.
func TestDNSAnswers(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			r := append([]byte(nil), buf[:n]...)
			r[2], r[3], r[7] = 0x81, 0x80, 1 // response, no error, one answer
			pc.WriteTo(r, from)
		}
	}()
	if !dnsAnswers(context.Background(), pc.LocalAddr().String()) {
		t.Error("a server that answers was reported as silent")
	}

	silent, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	start := time.Now()
	if dnsAnswers(context.Background(), silent.LocalAddr().String()) {
		t.Error("a silent server was reported as answering")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the probe took %v", time.Since(start))
	}
}
