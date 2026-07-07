package s3client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/fakes3"
)

// fakeSTS is a strict in-process AssumeRoleWithWebIdentity endpoint: it
// validates the request form and answers credentials derived from the
// presented token, so tests can see exactly which token was exchanged.
type fakeSTS struct {
	t       *testing.T
	roleARN string
	expiry  time.Time
	calls   atomic.Int64
	broken  string // "" | "denied" | "malformed" | "empty" | "badexpiry"
}

func (f *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("STS form parse: %v", err)
	}
	if r.Method != http.MethodPost {
		f.t.Errorf("STS method = %s", r.Method)
	}
	for field, want := range map[string]string{
		"Action":  "AssumeRoleWithWebIdentity",
		"Version": "2011-06-15",
		"RoleArn": f.roleARN,
	} {
		if got := r.PostForm.Get(field); got != want {
			f.t.Errorf("STS %s = %q, want %q", field, got, want)
		}
	}
	if r.PostForm.Get("RoleSessionName") == "" {
		f.t.Error("STS RoleSessionName empty")
	}
	token := r.PostForm.Get("WebIdentityToken")
	if token == "" {
		f.t.Error("STS WebIdentityToken empty")
	}

	switch f.broken {
	case "denied":
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<ErrorResponse><Error><Code>AccessDenied</Code><Message>no</Message></Error></ErrorResponse>`)
		return
	case "malformed":
		fmt.Fprint(w, `not xml <<<`)
		return
	case "empty":
		fmt.Fprint(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
		return
	case "badexpiry":
		fmt.Fprint(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>A</AccessKeyId><SecretAccessKey>S</SecretAccessKey><SessionToken>T</SessionToken><Expiration>not-a-time</Expiration></Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`)
		return
	}
	// Credentials derived from the token, so rotation is observable.
	// #nosec G705 -- test double emitting XML to a Go client, not a browser
	fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
		<AccessKeyId>AKID-%s</AccessKeyId>
		<SecretAccessKey>secret-%s</SecretAccessKey>
		<SessionToken>session-%s</SessionToken>
		<Expiration>%s</Expiration>
	</Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`,
		token, token, token, f.expiry.UTC().Format(time.RFC3339))
}

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestProvider(t *testing.T, sts *fakeSTS, tokenFile string) *WebIdentityProvider {
	t.Helper()
	srv := httptest.NewServer(sts)
	t.Cleanup(srv.Close)
	p, err := NewWebIdentityProvider(srv.URL, sts.roleARN, tokenFile, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewWebIdentityProviderValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewWebIdentityProvider("://bad", "arn", "f", ""); err == nil {
		t.Error("bad endpoint accepted")
	}
	if _, err := NewWebIdentityProvider("https://sts.example.com", "", "f", ""); err == nil {
		t.Error("missing role ARN accepted")
	}
	if _, err := NewWebIdentityProvider("https://sts.example.com", "arn", "", ""); err == nil {
		t.Error("missing token file accepted")
	}
	p, err := NewWebIdentityProvider("https://sts.example.com", "arn", "f", "")
	if err != nil || p.sessionName != "shpiel" {
		t.Errorf("default session name = %q, %v; want shpiel", p.sessionName, err)
	}
}

func TestWebIdentityExchangeAndCaching(t *testing.T) {
	t.Parallel()
	sts := &fakeSTS{t: t, roleARN: "arn:aws:iam::123:role/shpiel", expiry: time.Now().Add(time.Hour)}
	tokenFile := writeTokenFile(t, "tok-1\n") // trailing whitespace is trimmed
	p := newTestProvider(t, sts, tokenFile)

	creds, err := p.Credentials(context.Background())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.AccessKeyID != "AKID-tok-1" || creds.SecretAccessKey != "secret-tok-1" || creds.SessionToken != "session-tok-1" {
		t.Fatalf("creds = %+v", creds)
	}
	// A fresh session is served from cache: no second STS round-trip.
	if _, err := p.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sts.calls.Load(); got != 1 {
		t.Fatalf("STS calls = %d, want 1 (cached)", got)
	}
}

// TestWebIdentityRefresh pins the refresh boundary: credentials are
// reused until refreshSlack before expiry, and the rotated token file is
// re-read on refresh.
func TestWebIdentityRefresh(t *testing.T) {
	t.Parallel()
	expiry := time.Now().Add(time.Hour)
	sts := &fakeSTS{t: t, roleARN: "arn:aws:iam::123:role/shpiel", expiry: expiry}
	tokenFile := writeTokenFile(t, "tok-1")
	p := newTestProvider(t, sts, tokenFile)

	if _, err := p.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Just inside the slack window: still cached.
	p.now = func() time.Time { return expiry.Add(-refreshSlack - time.Second) }
	if _, err := p.Credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sts.calls.Load(); got != 1 {
		t.Fatalf("STS calls before slack = %d, want 1", got)
	}

	// Inside the slack window: refresh, picking up the rotated token.
	if err := os.WriteFile(tokenFile, []byte("tok-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	sts.expiry = expiry.Add(2 * time.Hour)
	p.now = func() time.Time { return expiry.Add(-refreshSlack + time.Second) }
	creds, err := p.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "AKID-tok-2" {
		t.Fatalf("refreshed creds = %+v, want rotated token", creds)
	}
	if got := sts.calls.Load(); got != 2 {
		t.Fatalf("STS calls after refresh = %d, want 2", got)
	}
}

func TestWebIdentityErrors(t *testing.T) {
	t.Parallel()
	roleARN := "arn:aws:iam::123:role/shpiel"

	t.Run("MissingTokenFile", func(t *testing.T) {
		t.Parallel()
		sts := &fakeSTS{t: t, roleARN: roleARN, expiry: time.Now().Add(time.Hour)}
		p := newTestProvider(t, sts, filepath.Join(t.TempDir(), "absent"))
		if _, err := p.Credentials(context.Background()); err == nil {
			t.Error("missing token file accepted")
		}
	})
	for _, mode := range []string{"denied", "malformed", "empty", "badexpiry"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			sts := &fakeSTS{t: t, roleARN: roleARN, expiry: time.Now().Add(time.Hour), broken: mode}
			p := newTestProvider(t, sts, writeTokenFile(t, "tok"))
			_, err := p.Credentials(context.Background())
			if err == nil {
				t.Fatalf("STS %s response accepted", mode)
			}
			if mode == "denied" && !strings.Contains(err.Error(), "AccessDenied") {
				t.Errorf("denied error = %v, want AccessDenied surfaced", err)
			}
		})
	}
}

// TestWebIdentityEndToEnd closes the loop: a token file is exchanged at
// the fake STS for session credentials, and those credentials SigV4-sign
// real bucket requests that fakes3's independent verifier accepts —
// including the session token, which must ride in the signed headers.
func TestWebIdentityEndToEnd(t *testing.T) {
	t.Parallel()
	sts := &fakeSTS{t: t, roleARN: "arn:aws:iam::123:role/shpiel", expiry: time.Now().Add(time.Hour)}
	p := newTestProvider(t, sts, writeTokenFile(t, "irsa"))

	fake := fakes3.New("models", "AKID-irsa", "secret-irsa")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c, err := New(Options{Endpoint: srv.URL, Bucket: "models", Region: "us-east-1", Provider: p})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	content := "ambient weights"
	if err := c.Put(ctx, "blobs/x", strings.NewReader(content), int64(len(content)), sha256hexOf(content)); err != nil {
		t.Fatalf("Put with web identity creds: %v", err)
	}
	rc, err := c.Get(ctx, "blobs/x", 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != content {
		t.Errorf("round-trip = %q", got)
	}
	if calls := sts.calls.Load(); calls != 1 {
		t.Errorf("STS calls = %d, want 1 (cached across requests)", calls)
	}
}

func sha256hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// errProvider always fails: provider errors must fail the request rather
// than fall back to anonymous.
type errProvider struct{}

func (errProvider) Credentials(context.Context) (Credentials, error) {
	return Credentials{}, errors.New("boom")
}

func TestDoSurfacesProviderErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request sent despite credential failure")
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Endpoint: srv.URL, Bucket: "b", Provider: errProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Head(context.Background(), "k"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Head with failing provider = %v, want provider error", err)
	}
}
