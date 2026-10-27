package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/algorhythmic/skald/internal/observability"
)

// Profile streams a bounded binary profile through the same Unix-only client.
func (c *Client) Profile(ctx context.Context, kind string, seconds int, out io.Writer) error {
	q := url.Values{"kind": {kind}, "seconds": {strconv.Itoa(seconds)}}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://skald/manage/profile?"+q.Encode(), nil)
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
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&failure) != nil || failure.Error == "" {
			return errors.New("profile_request_failed")
		}
		return errors.New(failure.Error)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		return errors.New("invalid_profile_response")
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, observability.MaxProfileBytes+1))
	if err != nil {
		return err
	}
	if n > observability.MaxProfileBytes {
		return errors.New("profile_too_large")
	}
	return nil
}
