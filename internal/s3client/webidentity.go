package s3client

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// CredentialsProvider supplies (possibly rotating) credentials. Zero-value
// Credentials mean anonymous.
type CredentialsProvider interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// StaticCredentials is the trivial provider: the same credentials forever.
type StaticCredentials Credentials

// Credentials implements CredentialsProvider.
func (s StaticCredentials) Credentials(context.Context) (Credentials, error) {
	return Credentials(s), nil
}

// refreshSlack is how long before expiry cached web-identity credentials
// are considered stale. STS sessions last an hour or more; five minutes
// absorbs clock skew and long uploads signed just before the refresh.
const refreshSlack = 5 * time.Minute

// WebIdentityProvider implements the AWS AssumeRoleWithWebIdentity flow —
// the ambient-credentials story on EKS (IRSA) and other OIDC-federated
// runtimes: a projected service-account token is exchanged at STS for
// rotating role credentials, so pods carry no static keys.
type WebIdentityProvider struct {
	stsEndpoint string
	roleARN     string
	tokenFile   string
	sessionName string
	http        *http.Client
	now         func() time.Time

	mu     sync.Mutex
	cached Credentials
	expiry time.Time
}

// NewWebIdentityProvider creates a provider. stsEndpoint has a scheme
// (https://sts.<region>.amazonaws.com); tokenFile is re-read on every
// refresh, so kubelet token rotation is picked up automatically.
func NewWebIdentityProvider(stsEndpoint, roleARN, tokenFile, sessionName string) (*WebIdentityProvider, error) {
	u, err := url.Parse(stsEndpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("s3client: invalid STS endpoint %q", stsEndpoint)
	}
	if roleARN == "" || tokenFile == "" {
		return nil, errors.New("s3client: web identity requires a role ARN and a token file")
	}
	if sessionName == "" {
		sessionName = "shpiel"
	}
	return &WebIdentityProvider{
		stsEndpoint: strings.TrimRight(stsEndpoint, "/"),
		roleARN:     roleARN,
		tokenFile:   tokenFile,
		sessionName: sessionName,
		http:        &http.Client{Transport: cloneDefaultTransport()},
		now:         time.Now,
	}, nil
}

// Credentials implements CredentialsProvider, refreshing through STS when
// the cached session is within refreshSlack of expiry.
func (w *WebIdentityProvider) Credentials(ctx context.Context) (Credentials, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cached.IsZero() && w.now().Before(w.expiry.Add(-refreshSlack)) {
		return w.cached, nil
	}
	creds, expiry, err := w.assume(ctx)
	if err != nil {
		return Credentials{}, err
	}
	w.cached, w.expiry = creds, expiry
	return creds, nil
}

// assume performs one AssumeRoleWithWebIdentity exchange. The request is
// deliberately unsigned: the web identity token IS the credential.
func (w *WebIdentityProvider) assume(ctx context.Context) (Credentials, time.Time, error) {
	token, err := os.ReadFile(w.tokenFile)
	if err != nil {
		return Credentials{}, time.Time{}, fmt.Errorf("s3client: reading web identity token: %w", err)
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {w.roleARN},
		"RoleSessionName":  {w.sessionName},
		"WebIdentityToken": {strings.TrimSpace(string(token))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.stsEndpoint+"/", strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := w.http.Do(req)
	if err != nil {
		return Credentials{}, time.Time{}, fmt.Errorf("s3client: STS request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Credentials{}, time.Time{}, fmt.Errorf("s3client: reading STS response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error xmlError `xml:"Error"`
		}
		_ = xml.Unmarshal(body, &e)
		if e.Error.Code != "" {
			return Credentials{}, time.Time{}, fmt.Errorf("s3client: STS: %s: %s (status %d)", e.Error.Code, e.Error.Message, resp.StatusCode)
		}
		return Credentials{}, time.Time{}, fmt.Errorf("s3client: STS status %d", resp.StatusCode)
	}

	var parsed struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
				SessionToken    string `xml:"SessionToken"`
				Expiration      string `xml:"Expiration"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleWithWebIdentityResult"`
	}
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return Credentials{}, time.Time{}, fmt.Errorf("s3client: decoding STS response: %w", err)
	}
	c := parsed.Result.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		return Credentials{}, time.Time{}, errors.New("s3client: STS response carries no credentials")
	}
	expiry, err := time.Parse(time.RFC3339, c.Expiration)
	if err != nil {
		return Credentials{}, time.Time{}, fmt.Errorf("s3client: STS expiration %q: %w", c.Expiration, err)
	}
	return Credentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		SessionToken:    c.SessionToken,
	}, expiry, nil
}
