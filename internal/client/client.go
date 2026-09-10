// Package client is the Unix-socket-only archive transport shared by CLI and TUI.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"skald/internal/archive"
)

type Client struct {
	http      *http.Client
	transport *http.Transport
}

func New(socket string) *Client {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}, DisableCompression: true}
	return &Client{transport: t, http: &http.Client{Transport: t, Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("unexpected_redirect") }}}
}
func (c *Client) Close() { c.transport.CloseIdleConnections() }
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://skald"+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("daemon_unavailable")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, archive.MaxResponseBytes+1))
	if err != nil {
		return errors.New("daemon_unavailable")
	}
	if len(b) > archive.MaxResponseBytes {
		return errors.New("response_too_large")
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &failure) != nil || failure.Error == "" {
			return errors.New("daemon_request_failed")
		}
		return errors.New(failure.Error)
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid_daemon_response")
	}
	return nil
}
