package xdns

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
	"net/url"
	"regexp"
	"strings"
	"time"
)

type DNSRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type DNS interface {
	Lookup(context.Context, string, Group) (DNSRecord, error)
	Update(context.Context, string, Group, DNSRecord, string) (DNSRecord, error)
}

type Cloudflare struct {
	base   string
	client *http.Client
}

func NewCloudflare() *Cloudflare {
	return &Cloudflare{base: "https://api.cloudflare.com/client/v4", client: &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type cloudError struct {
	Status int
	Detail string
	Retry  time.Duration
}

func (e *cloudError) Error() string { return fmt.Sprintf("Cloudflare HTTP %d: %s", e.Status, e.Detail) }

var cloudRecordID = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

func (c *Cloudflare) call(ctx context.Context, token, method, path string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return errors.New("could not build Cloudflare request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		action := "lookup"
		if method == http.MethodPatch {
			action = "update"
		}
		var dnsErr *net.DNSError
		var netErr net.Error
		var certErr *tls.CertificateVerificationError
		var opErr *net.OpError
		switch {
		case errors.Is(err, context.Canceled):
			return context.Canceled
		case errors.As(err, &dnsErr):
			return fmt.Errorf("Cloudflare %s: DNS resolution failed", action)
		case errors.As(err, &netErr) && netErr.Timeout():
			return fmt.Errorf("Cloudflare %s: connection or response timed out", action)
		case errors.As(err, &certErr):
			return fmt.Errorf("Cloudflare %s: TLS certificate verification failed", action)
		case errors.As(err, &opErr) && opErr.Op == "dial":
			return fmt.Errorf("Cloudflare %s: connection failed", action)
		default:
			return fmt.Errorf("Cloudflare %s: network request failed", action)
		}
	}
	defer resp.Body.Close()
	var result struct {
		Success bool
		Result  json.RawMessage
		Errors  []struct {
			Code    int
			Message string
		}
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&result); err != nil || !result.Success || resp.StatusCode/100 != 2 {
		detail := http.StatusText(resp.StatusCode)
		if len(result.Errors) > 0 {
			item := result.Errors[0]
			message := strings.ReplaceAll(item.Message, token, "[redacted]")
			message = strings.Join(strings.Fields(message), " ")
			if chars := []rune(message); len(chars) > 240 {
				message = string(chars[:240])
			}
			detail = fmt.Sprintf("%d · %s", item.Code, message)
		}
		var retry time.Duration
		if resp.StatusCode == http.StatusTooManyRequests {
			retry = time.Minute
			if seconds, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); err == nil {
				retry = min(max(seconds, time.Minute), time.Hour)
			}
		}
		return &cloudError{resp.StatusCode, detail, retry}
	}
	if err := json.Unmarshal(result.Result, out); err != nil {
		return errors.New("invalid Cloudflare record response")
	}
	return nil
}

func checkedRecord(record DNSRecord, g Group) (DNSRecord, error) {
	if !cloudRecordID.MatchString(record.ID) || !strings.EqualFold(strings.TrimSuffix(record.Name, "."), g.Record()) || record.Kind != g.Kind {
		return DNSRecord{}, errors.New("Cloudflare returned a different DNS record")
	}
	var err error
	record.Content, err = normalizeTarget(g.Kind, record.Content)
	return record, err
}

func (c *Cloudflare) Lookup(ctx context.Context, token string, g Group) (DNSRecord, error) {
	q := url.Values{"name.exact": {g.Record()}, "type": {g.Kind}, "per_page": {"5"}}
	var records []DNSRecord
	if err := c.call(ctx, token, http.MethodGet, "/zones/"+g.Zone+"/dns_records?"+q.Encode(), nil, &records); err != nil {
		return DNSRecord{}, err
	}
	if len(records) != 1 {
		return DNSRecord{}, fmt.Errorf("expected one %s record for %s; found %d", g.Kind, g.Record(), len(records))
	}
	return checkedRecord(records[0], g)
}

func (c *Cloudflare) Update(ctx context.Context, token string, g Group, record DNSRecord, content string) (DNSRecord, error) {
	if record.Content == content {
		return record, nil
	}
	body, err := json.Marshal(map[string]any{"content": content, "ttl": g.TTL, "proxied": g.Proxied})
	if err != nil {
		return DNSRecord{}, err
	}
	var result DNSRecord
	if err := c.call(ctx, token, http.MethodPatch, "/zones/"+g.Zone+"/dns_records/"+record.ID, body, &result); err != nil {
		return DNSRecord{}, err
	}
	result, err = checkedRecord(result, g)
	if err != nil {
		return DNSRecord{}, err
	}
	if result.ID != record.ID || result.Content != content || result.Proxied != g.Proxied || result.TTL != g.TTL {
		return DNSRecord{}, errors.New("Cloudflare did not confirm the requested content or record settings")
	}
	return result, nil
}
