//go:build !freebsd

package main

import (
	"fmt"
	"os"
)

// On a development machine the console is just the terminal and PS5
// notifications are printed.

type console struct{}

func newConsole() *console { return &console{} }

func (c *console) Printf(format string, args ...any) { fmt.Printf(format, args...) }

func redirectStdio(f *os.File) {}

func notify(format string, args ...any) {
	fmt.Printf("[notification] "+format+"\n", args...)
}
