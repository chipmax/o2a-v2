package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

// notifySystemd sends one datagram to NOTIFY_SOCKET when the process runs under
// systemd with NotifyAccess. It is a no-op otherwise, so the binary behaves the
// same outside systemd.
func notifySystemd(message string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(message))
}

// startWatchdog reports readiness and then pings the watchdog at half the
// interval systemd asked for, so a hung process is restarted instead of
// silently serving nothing.
func startWatchdog() {
	notifySystemd("READY=1")
	var micros int64
	if _, err := fmt.Sscanf(os.Getenv("WATCHDOG_USEC"), "%d", &micros); err != nil || micros <= 0 {
		return
	}
	interval := time.Duration(micros) * time.Microsecond / 2
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			notifySystemd("WATCHDOG=1")
		}
	}()
}
