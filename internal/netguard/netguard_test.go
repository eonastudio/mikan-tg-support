package netguard

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMetadataBlocked(t *testing.T) {
	d := ContextDialer(2 * time.Second)
	for _, addr := range []string{"169.254.169.254:80", "[fd00:ec2::254]:443"} {
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("the dial to %s went through", addr)
		}
		if !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("the dial to %s failed with %v, want the guard's error", addr, err)
		}
	}
}

func TestOthersAllowed(t *testing.T) {
	// A refused connection (nothing listens) must fail with the OS's error, not the
	// guard's: the guard is about the metadata addresses alone. Loopback is allowed
	// on purpose — a local LLM (Ollama, llama.cpp) lives there.
	d := ContextDialer(2 * time.Second)
	_, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if err == nil {
		t.Fatal("something listens on port 1")
	}
	if strings.Contains(err.Error(), "metadata") {
		t.Fatalf("the guard blocked loopback: %v", err)
	}
}

func TestClientHasNoRedirects(t *testing.T) {
	c := Client(time.Second, time.Second)
	if c.CheckRedirect == nil {
		t.Fatal("the client follows redirects")
	}
}
