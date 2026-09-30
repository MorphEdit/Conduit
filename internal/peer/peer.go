// Package peer is how one site talks to another: TLS pinned to the other
// site's certificate fingerprint, authenticated with this site's own secret.
package peer

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

func Fingerprint(certDER []byte) string {
	sum := sha256.Sum256(certDER)
	return hex.EncodeToString(sum[:])
}

var ErrFingerprint = errors.New("peer certificate does not match the pinned fingerprint")

var clients sync.Map

// PinnedClient returns an HTTP client that only talks to a server whose
// certificate has the given fingerprint. Hostnames are not checked: the pin
// is stronger than a name. An empty pin never matches.
func PinnedClient(fingerprint string, timeout time.Duration) *http.Client {
	want := strings.ToLower(fingerprint)
	key := want + "|" + timeout.String()
	if c, ok := clients.Load(key); ok {
		return c.(*http.Client)
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // replaced by the pin check below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if want == "" || len(cs.PeerCertificates) == 0 || Fingerprint(cs.PeerCertificates[0].Raw) != want {
					return ErrFingerprint
				}
				return nil
			},
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}
	c, _ := clients.LoadOrStore(key, &http.Client{Transport: tr, Timeout: timeout})
	return c.(*http.Client)
}

// Credentials identify this site to others: "Authorization: Conduit <id>:<secret>".
type Credentials struct {
	ID     string
	Secret string
}

func (c Credentials) Header() string { return "Conduit " + c.ID + ":" + c.Secret }

// ParseAuth splits an Authorization header into id and secret.
func ParseAuth(h string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(h, "Conduit ")
	if !found {
		return "", "", false
	}
	id, secret, ok = strings.Cut(rest, ":")
	return id, secret, ok && id != "" && secret != ""
}

// Target is another site: where it is and which certificate it must present.
type Target struct {
	URL         string
	Fingerprint string
}

// Do sends an authenticated request to t. creds may be zero for the few
// unauthenticated calls a joining site makes.
func Do(ctx context.Context, t Target, creds Credentials, method, path string, body io.Reader, timeout time.Duration) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if creds.ID != "" {
		req.Header.Set("Authorization", creds.Header())
	}
	return PinnedClient(t.Fingerprint, timeout).Do(req)
}
