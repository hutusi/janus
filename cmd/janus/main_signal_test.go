//go:build unix

package main

import (
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

// SIGTERM must take serve through its graceful path — listener stopped, drain
// run, nil returned — because that nil is what makes `systemctl stop` report
// success. Unix-only: Windows has no way to send SIGTERM to a process.
func TestServeShutsDownOnSignal(t *testing.T) {
	// Reserve an address by binding and releasing it; ListenAndServe needs the
	// port free again. The tiny window in which something else could take it
	// only makes the test fail loudly, never pass wrongly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	// A notifier and a status reporter are configured so the shutdown drain
	// closes real ones, not the nil fast paths.
	cfg := "notifications:\n  - url: \"https://hooks.example.com/x\"\n" +
		"gitlab_api_token: tok\ngitlab_url: \"https://gitlab.example.com\"\n"
	c, err := buildServe(buildServeArgs(t, writeConfig(t, cfg), "--addr", addr), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(c) }()

	// The signal handler is installed before the listener starts, so once
	// /healthz answers, a SIGTERM cannot race serve's setup — and cannot kill
	// the test process either.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up on %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve after SIGTERM = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return within 10s of SIGTERM")
	}
}
