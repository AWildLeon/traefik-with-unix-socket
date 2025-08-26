package service

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/traefik/traefik/v3/pkg/config/dynamic"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

type h2cTransportWrapper struct {
	*http2.Transport
}

func (t *h2cTransportWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	return t.Transport.RoundTrip(req)
}

type unixTransport struct {
	http.Transport
}

func (t *unixTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = req.URL.Scheme[len("unix+"):]
	return t.Transport.RoundTrip(req)
}

func newSmartRoundTripper(transport *http.Transport, forwardingTimeouts *dynamic.ForwardingTimeouts) (*smartRoundTripper, error) {
	transportHTTP1 := transport.Clone()

	transportHTTP2, err := http2.ConfigureTransports(transport)
	if err != nil {
		return nil, err
	}

	if forwardingTimeouts != nil {
		transportHTTP2.ReadIdleTimeout = time.Duration(forwardingTimeouts.ReadIdleTimeout)
		transportHTTP2.PingTimeout = time.Duration(forwardingTimeouts.PingTimeout)
	}

	transportH2C := &h2cTransportWrapper{
		Transport: &http2.Transport{
			DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
				return net.Dial(network, addr)
			},
			AllowHTTP: true,
		},
	}

	if forwardingTimeouts != nil {
		transportH2C.ReadIdleTimeout = time.Duration(forwardingTimeouts.ReadIdleTimeout)
		transportH2C.PingTimeout = time.Duration(forwardingTimeouts.PingTimeout)
	}

	transport.RegisterProtocol("h2c", transportH2C)

	// Unix socket support for HTTP/1
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport.RegisterProtocol("unix+http", &unixTransport{
		Transport: http.Transport{
			MaxIdleConnsPerHost:   transport.MaxIdleConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DialContext: func(ctx context.Context, netw, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				return dialer.DialContext(ctx, "unix", host)
			},
		},
	})

	// Unix socket support for HTTP/2 (H2C)
	transport.RegisterProtocol("unix+h2c", &h2cTransportWrapper{
		Transport: &http2.Transport{
			DialTLS: func(netw, addr string, cfg *tls.Config) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				return net.Dial("unix", host)
			},
			AllowHTTP: true,
		},
	})

	return &smartRoundTripper{
		http2: transport,
		http:  transportHTTP1,
	}, nil
}

// smartRoundTripper implements RoundTrip while making sure that HTTP/2 is not used
// with protocols that start with a Connection Upgrade, such as SPDY or Websocket.
type smartRoundTripper struct {
	http2 *http.Transport
	http  *http.Transport
}

func (m *smartRoundTripper) Clone() http.RoundTripper {
	h := m.http.Clone()
	h2 := m.http2.Clone()
	return &smartRoundTripper{http: h, http2: h2}
}

func (m *smartRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// If we have a connection upgrade, we don't use HTTP/2
	if httpguts.HeaderValuesContainsToken(req.Header["Connection"], "Upgrade") {
		return m.http.RoundTrip(req)
	}

	return m.http2.RoundTrip(req)
}
