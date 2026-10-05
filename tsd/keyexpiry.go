package main

import (
	"context"
	"fmt"
	"time"
)

// A device's Tailscale key expires after a while (180 days unless the
// tailnet says otherwise or expiry is turned off for the device). When that
// happens to a console nobody is looking at, it simply drops off the tailnet.
// The status page shows the date; this warns on screen as it gets close.

// keyWarnDays are the points, in days before the expiry, at which the user
// is told on screen. The status page warns from the first of them on.
var keyWarnDays = []int{14, 3, 1}

// daysLeft is the number of whole days from now until expiry, negative once
// it has passed.
func daysLeft(now, expiry time.Time) int {
	d := expiry.Sub(now)
	if d < 0 {
		return -1
	}
	return int(d / (24 * time.Hour))
}

// keyWarnStage returns the index of the last warning point that has been
// reached, or -1 if the expiry is still further away than all of them.
func keyWarnStage(now, expiry time.Time) int {
	left := expiry.Sub(now)
	stage := -1
	for i, days := range keyWarnDays {
		if left <= time.Duration(days)*24*time.Hour {
			stage = i
		}
	}
	return stage
}

// keyExpiry returns when this console's key expires; the zero time if it
// does not expire or is not known.
func (d *daemon) keyExpiry(ctx context.Context) time.Time {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := d.lc.StatusWithoutPeers(ctx)
	if err != nil || st.Self == nil || st.Self.KeyExpiry == nil {
		return time.Time{}
	}
	return *st.Self.KeyExpiry
}

// watchKeyExpiry tells the user on screen when the key is about to expire:
// once for each warning point reached while this process runs.
func (d *daemon) watchKeyExpiry(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	warned := -1
	first := time.After(2 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-first:
		case <-ticker.C:
		}
		d.mu.Lock()
		running := d.state == "Running"
		d.mu.Unlock()
		if !running {
			continue
		}
		expiry := d.keyExpiry(ctx)
		if expiry.IsZero() {
			warned = -1
			continue
		}
		now := time.Now()
		stage := keyWarnStage(now, expiry)
		if stage <= warned || !expiry.After(now) {
			if stage < warned {
				warned = stage // the key was renewed
			}
			continue
		}
		warned = stage
		left := "in less than a day"
		if n := daysLeft(now, expiry); n >= 1 {
			left = "in " + plural(n, "day")
		}
		d.logf("this console's Tailscale key expires %s (%s)", left, expiry.Local().Format("2006-01-02"))
		notify("Tailscale: this PS5's key expires %s.\nLog in again or turn off key expiry.\n%s", left, d.webURL())
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
