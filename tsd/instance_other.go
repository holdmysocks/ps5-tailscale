//go:build !freebsd

package main

// On a development machine there is no stale-instance handling.

func stopRecordedInstance(pidFile string) int { return 0 }

func recordInstance(pidFile string) error { return nil }
