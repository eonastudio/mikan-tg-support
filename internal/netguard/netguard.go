// Package netguard builds the outbound HTTP transports of the addon: both external
// APIs it talks to (Telegram and an OpenAI-compatible LLM) are admin-configured URLs,
// and a container on the panel's bridge must never be talked into reaching a cloud
// metadata endpoint through one of them. Everything else (including loopback, where a
// local LLM may live) stays allowed: the admin chooses the addresses, and the ufw
// rules of the host already keep the bridge away from the rest of the machine.
package netguard

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// blockedIPs are the cloud metadata endpoints (AWS/GCP/Azure/Oracle) and their IPv6
// twins. A legitimate Telegram or LLM API is never there.
var blockedIPs = []string{
	"169.254.169.254",
	"fd00:ec2::254",
}

var errBlocked = errors.New("netguard: the address is a cloud metadata endpoint")

// control refuses a dial to a blocked address, whatever name resolved to it (so a
// DNS name pointing at the metadata IP does not slip through either).
func control(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	for _, b := range blockedIPs {
		if ip.Equal(net.ParseIP(b)) {
			return errBlocked
		}
	}
	return nil
}

// Transport is an http.Transport for the addon's outbound calls: no redirects (the
// caller decides), sane timeouts, and the metadata guard on every dial.
func Transport(dialTimeout time.Duration) *http.Transport {
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: control}
	return &http.Transport{
		DialContext:           d.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          8,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       90 * time.Second,
	}
}

// Client is an outbound http.Client with the guard and no redirects.
func Client(timeout, dialTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: Transport(dialTimeout),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ContextDialer is exported for tests that want the guard on their own dialer.
func ContextDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: control}
}
