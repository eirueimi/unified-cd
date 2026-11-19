package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// redirectRig serves a "controller" at ci.example.test that redirects the
// artifact download to a "store" at storeHost, over one custom dialer.
func redirectRig(t *testing.T, storeHost string, payload []byte) (client *http.Client, storeAuth func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.URL.Query().Get("X-Amz-Signature") != "sig" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(store.Close)
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "http://"+storeHost+"/artifacts/run1/build.tar.gz?X-Amz-Signature=sig", http.StatusFound)
	}))
	t.Cleanup(ctrl.Close)

	addrs := map[string]string{
		"ci.example.test:80": strings.TrimPrefix(ctrl.URL, "http://"),
		storeHost + ":80":    strings.TrimPrefix(store.URL, "http://"),
	}
	d := &net.Dialer{}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		real, ok := addrs[addr]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: addr}
		}
		return d.DialContext(ctx, network, real)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func runDownload(t *testing.T, client *http.Client, dest string) (string, error) {
	t.Helper()
	resolve := func() (Config, error) { return Config{Server: "http://ci.example.test", Token: "tok"}, nil }
	cmd := newArtifactCmdWithClient(resolve, client)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"download", "run1", "build", "--dest", dest})
	err := cmd.Execute()
	return out.String(), err
}

func TestArtifactDownload_CLIFollowsRedirect(t *testing.T) {
	payload := tarZstd(t, "hello.txt", "hi")
	client, _ := redirectRig(t, "store.other.test", payload)
	dest := t.TempDir()
	if _, err := runDownload(t, client, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("extracted file = %q, %v", b, err)
	}
}

func TestArtifactDownload_CLIDropsAuthorizationOnSubdomainRedirect(t *testing.T) {
	payload := tarZstd(t, "hello.txt", "hi")
	client, storeAuth := redirectRig(t, "s3.ci.example.test", payload)
	if _, err := runDownload(t, client, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got := storeAuth()
	if len(got) != 1 {
		t.Fatalf("store requests = %d, want 1", len(got))
	}
	if got[0] != "" {
		t.Fatalf("store saw Authorization %q; the bearer token must not leave the controller host", got[0])
	}
}
