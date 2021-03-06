// Package apns2 is a go Apple Push Notification Service (APNs) provider that
// allows you to send remote notifications to your iOS, tvOS, and OS X
// apps, using the new APNs HTTP/2 network protocol.
package apns2

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"crypto/rand"

	"sync"

	"github.com/gngeorgiev/apns2/token"
	"golang.org/x/net/http2"
)

// Apple HTTP/2 Development & Production urls
const (
	HostDevelopment = "https://api.sandbox.push.apple.com"
	HostProduction  = "https://api.push.apple.com"
)

var (
	// DefaultHost is a mutable var for testing purposes
	DefaultHost = HostDevelopment

	// TLSDialTimeout is the maximum amount of time a dial will wait for a connect
	// to complete.
	TLSDialTimeout = 20 * time.Second
	// HTTPClientTimeout specifies a time limit for requests made by the
	// HTTPClient. The timeout includes connection time, any redirects,
	// and reading the response body.
	HTTPClientTimeout = 60 * time.Second
	// TCPKeepAlive specifies the keep-alive period for an active network
	// connection. If zero, keep-alives are not enabled.
	TCPKeepAlive = 60 * time.Second

	// ErrPingingStopped is returned when the pinging of a client is stopped
	ErrPingingStopped = errors.New("pinging stopped")
)

// DialTLS is the default dial function for creating TLS connections for
// non-proxied HTTPS requests.
var DialTLS = func(network, addr string, cfg *tls.Config) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   TLSDialTimeout,
		KeepAlive: TCPKeepAlive,
	}
	return tls.DialWithDialer(dialer, network, addr, cfg)
}

// Client represents a connection with the APNs
type Client struct {
	Host        string
	Certificate tls.Certificate
	Token       *token.Token
	HTTPClient  *http.Client

	pingingMutex sync.Mutex
	pinging      bool

	stopPinging  chan struct{}
	pingInterval time.Duration

	connMutex sync.Mutex
	conn      *tls.Conn
}

// A Context carries a deadline, a cancellation signal, and other values across
// API boundaries. Context's methods may be called by multiple goroutines
// simultaneously.
type Context interface {
	context.Context
}

type connectionCloser interface {
	CloseIdleConnections()
}

func newDefaultClient() *Client {
	client := &Client{}
	client.Host = DefaultHost
	client.stopPinging = make(chan struct{})
	client.HTTPClient = &http.Client{
		Timeout: HTTPClientTimeout,
	}

	return client
}

func newClientWithHttp2Transport() *Client {
	client := newDefaultClient()
	t := &http2.Transport{}
	t.DialTLS = func(network, addr string, cfg *tls.Config) (net.Conn, error) {
		conn, err := DialTLS(network, addr, cfg)
		if err != nil {
			return nil, err
		}

		client.setConnection(conn.(*tls.Conn))
		return conn, nil
	}

	client.HTTPClient.Transport = t

	return client
}

// NewClient returns a new Client with an underlying http.Client configured with
// the correct APNs HTTP/2 transport settings. It does not connect to the APNs
// until the first Notification is sent via the Push method.
//
// As per the Apple APNs Provider API, you should keep a handle on this client
// so that you can keep your connections with APNs open across multiple
// notifications; don’t repeatedly open and close connections. APNs treats rapid
// connection and disconnection as a denial-of-service attack.
//
// If your use case involves multiple long-lived connections, consider using
// the ClientManager, which manages clients for you.
func NewClient(certificate tls.Certificate) *Client {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
	}
	if len(certificate.Certificate) > 0 {
		tlsConfig.BuildNameToCertificate()
	}

	client := newClientWithHttp2Transport()
	client.HTTPClient.Transport.(*http2.Transport).TLSClientConfig = tlsConfig
	client.Certificate = certificate

	return client
}

//EnablePinging starts pinging the last opened connection. This way, there's always one connection
//kept alive which allows for quick send of push notifications
func (c *Client) EnablePinging(pingInterval time.Duration, pingErrorCh chan error) {
	//lets make sure that the old goroutine has exited in case the user calls this method multiple times
	c.DisablePinging()

	c.pingingMutex.Lock()
	defer c.pingingMutex.Unlock()

	c.pinging = true
	c.pingInterval = pingInterval

	go func() {
		t := time.NewTicker(pingInterval)
		var framer *http2.Framer
		for {
			select {
			case <-t.C:
				conn := c.getConnection()
				if conn == nil {
					continue
				}

				if framer == nil {
					framer = http2.NewFramer(conn, conn)
				}

				var p [8]byte
				rand.Read(p[:])
				err := framer.WritePing(false, p)
				if err != nil {
					c.setConnection(nil)
					framer = nil
					if pingErrorCh != nil {
						pingErrorCh <- err
					}
				}
			case <-c.stopPinging:
				t.Stop()
				framer = nil
				if pingErrorCh != nil {
					pingErrorCh <- ErrPingingStopped
				}
				return
			}
		}
	}()
}

//DisablePinging stops the pinging
func (c *Client) DisablePinging() {
	c.pingingMutex.Lock()
	defer c.pingingMutex.Unlock()

	if c.pinging {
		c.stopPinging <- struct{}{}
	}

	c.pinging = false
}

// NewTokenClient returns a new Client with an underlying http.Client configured
// with the correct APNs HTTP/2 transport settings. It does not connect to the APNs
// until the first Notification is sent via the Push method.
//
// As per the Apple APNs Provider API, you should keep a handle on this client
// so that you can keep your connections with APNs open across multiple
// notifications; don’t repeatedly open and close connections. APNs treats rapid
// connection and disconnection as a denial-of-service attack.
func NewTokenClient(token *token.Token) *Client {
	client := newClientWithHttp2Transport()
	client.Token = token
	return client
}

// Development sets the Client to use the APNs development push endpoint.
func (c *Client) Development() *Client {
	c.Host = HostDevelopment
	return c
}

// Production sets the Client to use the APNs production push endpoint.
func (c *Client) Production() *Client {
	c.Host = HostProduction
	return c
}

//IsPinging returns whether the client is currently pinging the APNS servers
func (c *Client) IsPinging() bool {
	c.pingingMutex.Lock()
	defer c.pingingMutex.Unlock()
	return c.pinging
}

//GetPingInterval returns the ping interval, if set on EnablePinging
func (c *Client) GetPingInterval() time.Duration {
	return c.pingInterval
}

// Push sends a Notification to the APNs gateway. If the underlying http.Client
// is not currently connected, this method will attempt to reconnect
// transparently before sending the notification. It will return a Response
// indicating whether the notification was accepted or rejected by the APNs
// gateway, or an error if something goes wrong.
//
// Use PushWithContext if you need better cancellation and timeout control.
func (c *Client) Push(n *Notification) (*Response, error) {
	return c.PushWithContext(nil, n)
}

// PushWithContext sends a Notification to the APNs gateway. Context carries a
// deadline and a cancellation signal and allows you to close long running
// requests when the context timeout is exceeded. Context can be nil, for
// backwards compatibility.
//
// If the underlying http.Client is not currently connected, this method will
// attempt to reconnect transparently before sending the notification. It will
// return a Response indicating whether the notification was accepted or
// rejected by the APNs gateway, or an error if something goes wrong.
func (c *Client) PushWithContext(ctx Context, n *Notification) (*Response, error) {
	return c.PushWithHostContext(ctx, c.Host, n)
}

//PushWithHostContext sends a push with the specified host and context
//useful when one client needs to send dev and prod notifications in a concurrent environment
func (c *Client) PushWithHostContext(ctx Context, host string, n *Notification) (*Response, error) {
	payload, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%v/3/device/%v", host, n.DeviceToken)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}

	if c.Token != nil {
		c.setTokenHeader(req)
	}

	setHeaders(req, n)

	httpRes, err := c.requestWithContext(ctx, req)
	if err != nil {
		return nil, err
	}
	defer httpRes.Body.Close()

	response := &Response{}
	response.StatusCode = httpRes.StatusCode
	response.ApnsID = httpRes.Header.Get("apns-id")

	decoder := json.NewDecoder(httpRes.Body)
	if err := decoder.Decode(&response); err != nil && err != io.EOF {
		return &Response{}, err
	}

	return response, nil
}

// CloseIdleConnections closes any underlying connections which were previously
// connected from previous requests but are now sitting idle. It will not
// interrupt any connections currently in use.
func (c *Client) CloseIdleConnections() {
	c.HTTPClient.Transport.(connectionCloser).CloseIdleConnections()
}

func (c *Client) setTokenHeader(r *http.Request) {
	bearer := c.Token.GenerateIfExpired()
	r.Header.Set("authorization", fmt.Sprintf("bearer %v", bearer))
}

func (c *Client) getConnection() *tls.Conn {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	return c.conn
}

func (c *Client) setConnection(conn *tls.Conn) {
	c.connMutex.Lock()
	c.conn = conn
	c.connMutex.Unlock()
}

func setHeaders(r *http.Request, n *Notification) {
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if n.Topic != "" {
		r.Header.Set("apns-topic", n.Topic)
	}
	if n.ApnsID != "" {
		r.Header.Set("apns-id", n.ApnsID)
	}
	if n.CollapseID != "" {
		r.Header.Set("apns-collapse-id", n.CollapseID)
	}
	if n.Priority > 0 {
		r.Header.Set("apns-priority", fmt.Sprintf("%v", n.Priority))
	}
	if !n.Expiration.IsZero() {
		r.Header.Set("apns-expiration", fmt.Sprintf("%v", n.Expiration.Unix()))
	}
	if n.PushType != "" {
		r.Header.Set("apns-push-type", string(n.PushType))
	} else {
		r.Header.Set("apns-push-type", string(PushTypeAlert))
	}

}

func (c *Client) requestWithContext(ctx Context, req *http.Request) (*http.Response, error) {
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	return c.HTTPClient.Do(req)
}
