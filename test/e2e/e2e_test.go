//go:build e2e

// Package e2e proves the M0 exit criterion with real components: the actual
// shpiel binary (built from source, started via its CLI with a config
// file), a hermetic in-process fakehub as upstream, and an unmodified
// huggingface_hub / hf CLI client running in Docker with only HF_ENDPOINT
// pointed at Shpiel.
//
// Run via: task e2e   (requires Docker; skipped otherwise unless
// SHPIEL_E2E_REQUIRE=1)
package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/loewenthal-corp/shpiel/internal/fakehub"
)

const clientImage = "shpiel-e2e-hf-client"

func TestRealHFClientAgainstShpiel(t *testing.T) {
	requireDocker(t)
	repoRoot := repoRoot(t)

	// --- Fixture ---
	files := map[string][]byte{
		"config.json":        []byte(`{"model_type":"e2e","hidden_size":32}`),
		"tokenizer.json":     []byte(`{"version":"1.0"}`),
		"model.safetensors":  deterministicBytes(2 << 20), // 2 MiB, LFS
		"vae/decoder.bin":    deterministicBytes(128 << 10),
	}
	hub := fakehub.New()
	commit := hub.AddModel("fixtures/e2e-model", files)
	hubSrv := httptest.NewServer(hub.Handler())
	defer hubSrv.Close()

	// --- Build and start the real shpiel binary ---
	bin := buildShpiel(t, repoRoot)
	apiPort, metricsPort := freePort(t), freePort(t)
	dataDir := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, cfgPath, fmt.Sprintf(`---
listen:
  api: "0.0.0.0:%d"
  metrics: "127.0.0.1:%d"
upstream:
  huggingface:
    endpoint: %s
    pull_through: true
    refresh_interval: 1m
backends:
  cache:
    type: fs
    path: %s
routes:
  - match: "*"
    primary: cache
log:
  level: debug
  format: text
`, apiPort, metricsPort, hubSrv.URL, dataDir))

	shpiel := exec.Command(bin, "serve", "--config", cfgPath)
	shpiel.Stdout = testWriter{t, "shpiel"}
	shpiel.Stderr = testWriter{t, "shpiel"}
	if err := shpiel.Start(); err != nil {
		t.Fatalf("starting shpiel: %v", err)
	}
	defer func() {
		_ = shpiel.Process.Kill()
		_ = shpiel.Wait()
	}()
	waitReady(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", apiPort))

	// --- Run the real HF client in Docker ---
	buildClientImage(t, repoRoot)

	expected := map[string]map[string]any{}
	for path, content := range files {
		sum := sha256.Sum256(content)
		expected[path] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": len(content)}
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}

	out := runClient(t, repoRoot, []string{
		"-e", fmt.Sprintf("HF_ENDPOINT=http://host.docker.internal:%d", apiPort),
		"-e", "E2E_REPO=fixtures/e2e-model",
		"-e", "E2E_COMMIT=" + commit,
		"-e", "E2E_FILES=" + string(expectedJSON),
		"-e", "HF_HUB_DISABLE_TELEMETRY=1",
	}, "/client/verify.py")

	if !strings.Contains(out, "E2E_OK") {
		t.Fatalf("client verification did not reach E2E_OK:\n%s", out)
	}

	// --- Observability: the pull-through actually happened and was counted ---
	metrics := httpGetBody(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort))
	for _, want := range []string{
		`shpiel_pullthrough_fetches_total{kind="blob",outcome="ok"}`,
		`shpiel_pullthrough_fetches_total{kind="manifest",outcome="ok"}`,
		`shpiel_http_requests_total`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics missing %s", want)
		}
	}

	// --- Second client run: everything served from cache, no new upstream blob fetches ---
	blobFetchesBefore := hub.Requests("GET", "/cdn/") + hub.Requests("GET", "/fixtures/e2e-model/resolve")
	out = runClient(t, repoRoot, []string{
		"-e", fmt.Sprintf("HF_ENDPOINT=http://host.docker.internal:%d", apiPort),
		"-e", "E2E_REPO=fixtures/e2e-model",
		"-e", "E2E_COMMIT=" + commit,
		"-e", "E2E_FILES=" + string(expectedJSON),
		"-e", "HF_HUB_DISABLE_TELEMETRY=1",
	}, "/client/verify.py")
	if !strings.Contains(out, "E2E_OK") {
		t.Fatalf("second client run failed:\n%s", out)
	}
	blobFetchesAfter := hub.Requests("GET", "/cdn/") + hub.Requests("GET", "/fixtures/e2e-model/resolve")
	if blobFetchesAfter != blobFetchesBefore {
		t.Errorf("second run hit upstream for blobs (%d -> %d fetches); cache is not serving", blobFetchesBefore, blobFetchesAfter)
	}
}

// --- helpers ---

func requireDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "version").Run(); err != nil {
		if os.Getenv("SHPIEL_E2E_REQUIRE") == "1" {
			t.Fatalf("docker is required (SHPIEL_E2E_REQUIRE=1) but unavailable: %v", err)
		}
		t.Skipf("docker unavailable, skipping e2e: %v", err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func buildShpiel(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "shpiel")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/shpiel")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building shpiel: %v\n%s", err, out)
	}
	return bin
}

func buildClientImage(t *testing.T, repoRoot string) {
	t.Helper()
	cmd := exec.Command("docker", "build",
		"-t", clientImage,
		"-f", filepath.Join(repoRoot, "containers", "hf-client.Dockerfile"),
		filepath.Join(repoRoot, "containers"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building client image: %v\n%s", err, out)
	}
}

func runClient(t *testing.T, repoRoot string, env []string, script string) string {
	t.Helper()
	args := []string{"run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-v", filepath.Join(repoRoot, "test", "e2e", "client") + ":/client:ro",
	}
	args = append(args, env...)
	args = append(args, clientImage, script)
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	t.Logf("client output:\n%s", out)
	if err != nil {
		t.Fatalf("client run failed: %v", err)
	}
	return string(out)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("shpiel never became ready at %s", url)
}

func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type testWriter struct {
	t    *testing.T
	name string
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("[%s] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// deterministicBytes generates n stable pseudo-random bytes (xorshift32).
func deterministicBytes(n int) []byte {
	buf := make([]byte, n)
	state := uint32(0xe2e)
	for i := range buf {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		buf[i] = byte(state)
	}
	return buf
}
