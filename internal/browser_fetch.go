package internal

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/HeapOfChaos/goondvr/server"
)

type browserHelperResponse struct {
	Body  string `json:"body"`
	Error string `json:"error,omitempty"`
}

type browserFetchResult struct {
	Status   int    `json:"status"`
	Body     string `json:"body"`
	FinalURL string `json:"final_url"`
	Title    string `json:"title"`
}

type devToolsTarget struct {
	ID                   string `json:"id"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type devToolsVersion struct {
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type cdpClient struct {
	conn   net.Conn
	reader *bufio.Reader
	nextID int64
}

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func GetWithBrowserFallback(ctx context.Context, req *Req, targetURL string) (string, error) {
	body, err := req.Get(ctx, targetURL)
	if err == nil || !errors.Is(err, ErrCloudflareBlocked) {
		return body, err
	}
	if server.Config == nil || server.Config.BrowserMode == "off" {
		return "", err
	}
	return BrowserFetch(ctx, targetURL)
}

func BrowserFetch(ctx context.Context, targetURL string) (string, error) {
	switch server.Config.BrowserMode {
	case "local":
		return browserFetchLocal(ctx, targetURL)
	case "remote":
		return browserFetchRemote(ctx, targetURL)
	default:
		return "", ErrCloudflareBlocked
	}
}

func BrowserAPIGet(ctx context.Context, pageURL, targetURL string) (string, error) {
	switch server.Config.BrowserMode {
	case "local":
		result, err := browserAPIGetLocal(ctx, pageURL, targetURL)
		if err != nil {
			return "", err
		}
		return result.Body, nil
	case "remote":
		return browserAPIGetRemote(ctx, pageURL, targetURL)
	default:
		return "", ErrCloudflareBlocked
	}
}

func browserFetchLocal(ctx context.Context, targetURL string) (string, error) {
	if err := os.MkdirAll(filepath.Clean(server.Config.BrowserProfileDir), 0700); err != nil {
		return "", fmt.Errorf("mkdir browser profile dir: %w", err)
	}
	if err := ensureBrowserSession(ctx); err != nil {
		return "", err
	}
	if server.Config.Debug {
		fmt.Printf("[DEBUG] browser fetch via devtools: %s\n", targetURL)
	}
	return fetchHTMLViaDevTools(ctx, targetURL)
}

func browserAPIGetLocal(ctx context.Context, pageURL, targetURL string) (*browserFetchResult, error) {
	if err := os.MkdirAll(filepath.Clean(server.Config.BrowserProfileDir), 0700); err != nil {
		return nil, fmt.Errorf("mkdir browser profile dir: %w", err)
	}
	if err := ensureBrowserSession(ctx); err != nil {
		return nil, err
	}
	if server.Config.Debug {
		fmt.Printf("[DEBUG] browser API fetch via devtools: page=%s target=%s\n", pageURL, targetURL)
	}
	return fetchAPIViaDevTools(ctx, pageURL, targetURL)
}

func browserFetchRemote(ctx context.Context, targetURL string) (string, error) {
	if server.Config.BrowserHelperURL == "" {
		return "", fmt.Errorf("browser helper URL is empty")
	}

	fetchURL := strings.TrimRight(server.Config.BrowserHelperURL, "/") + "/fetch?url=" + url.QueryEscape(targetURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return "", fmt.Errorf("new browser helper request: %w", err)
	}
	if server.Config.BrowserHelperToken != "" {
		req.Header.Set("Authorization", "Bearer "+server.Config.BrowserHelperToken)
	}

	resp, err := CreateTransport().RoundTrip(req)
	if err != nil {
		return "", fmt.Errorf("call browser helper: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read browser helper response: %w", err)
	}

	var payload browserHelperResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decode browser helper response: %w", err)
	}
	if payload.Error != "" {
		if payload.Error == ErrCloudflareBlocked.Error() {
			return "", ErrCloudflareBlocked
		}
		return "", fmt.Errorf("browser helper: %s", payload.Error)
	}
	return payload.Body, nil
}

func browserAPIGetRemote(ctx context.Context, pageURL, targetURL string) (string, error) {
	if server.Config.BrowserHelperURL == "" {
		return "", fmt.Errorf("browser helper URL is empty")
	}

	fetchURL := strings.TrimRight(server.Config.BrowserHelperURL, "/") + "/api-fetch?page_url=" + url.QueryEscape(pageURL) + "&url=" + url.QueryEscape(targetURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return "", fmt.Errorf("new browser helper api request: %w", err)
	}
	if server.Config.BrowserHelperToken != "" {
		req.Header.Set("Authorization", "Bearer "+server.Config.BrowserHelperToken)
	}

	resp, err := CreateTransport().RoundTrip(req)
	if err != nil {
		return "", fmt.Errorf("call browser helper api: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read browser helper api response: %w", err)
	}

	var payload browserHelperResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("decode browser helper api response: %w", err)
	}
	if payload.Error != "" {
		if payload.Error == ErrCloudflareBlocked.Error() {
			return "", ErrCloudflareBlocked
		}
		return "", fmt.Errorf("browser helper: %s", payload.Error)
	}
	return payload.Body, nil
}

func RunBrowserHelper(bindAddr string) error {
	if server.Config.BrowserBootstrap {
		if err := LaunchBrowserBootstrap(server.Config.BrowserBootstrapURL); err != nil {
			return fmt.Errorf("launch browser bootstrap: %w", err)
		}
		if err := waitForBrowserSession(context.Background(), 15*time.Second); err != nil {
			return fmt.Errorf("wait for browser session: %w", err)
		}
	} else {
		if err := ensureBrowserSession(context.Background()); err != nil {
			return fmt.Errorf("ensure browser session: %w", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/fetch", func(w http.ResponseWriter, r *http.Request) {
		if server.Config.BrowserHelperToken != "" {
			if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != server.Config.BrowserHelperToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		targetURL := r.URL.Query().Get("url")
		if targetURL == "" {
			http.Error(w, "missing url", http.StatusBadRequest)
			return
		}
		if server.Config.Debug {
			fmt.Printf("[DEBUG] browser helper fetch request: %s\n", targetURL)
		}

		body, err := browserFetchLocal(r.Context(), targetURL)
		payload := browserHelperResponse{Body: body}
		if err != nil {
			payload.Error = err.Error()
			if server.Config.Debug {
				fmt.Printf("[DEBUG] browser helper fetch error for %s: %v\n", targetURL, err)
			}
		} else if server.Config.Debug {
			fmt.Printf("[DEBUG] browser helper fetch ok for %s: %d bytes\n", targetURL, len(body))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/api-fetch", func(w http.ResponseWriter, r *http.Request) {
		if server.Config.BrowserHelperToken != "" {
			if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != server.Config.BrowserHelperToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		targetURL := r.URL.Query().Get("url")
		pageURL := r.URL.Query().Get("page_url")
		if targetURL == "" || pageURL == "" {
			http.Error(w, "missing url or page_url", http.StatusBadRequest)
			return
		}
		if server.Config.Debug {
			fmt.Printf("[DEBUG] browser helper api fetch request: page=%s target=%s\n", pageURL, targetURL)
		}

		result, err := browserAPIGetLocal(r.Context(), pageURL, targetURL)
		payload := browserHelperResponse{}
		if err != nil {
			payload.Error = err.Error()
			if server.Config.Debug {
				fmt.Printf("[DEBUG] browser helper api fetch error for %s: %v\n", targetURL, err)
			}
		} else {
			payload.Body = result.Body
			if server.Config.Debug {
				fmt.Printf("[DEBUG] browser helper api fetch ok for %s: status=%d final=%s title=%q bytes=%d\n", targetURL, result.Status, result.FinalURL, result.Title, len(result.Body))
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})

	fmt.Printf("Browser helper listening on %s\n", bindAddr)
	fmt.Printf("Using browser path %s with profile %s\n", server.Config.BrowserPath, server.Config.BrowserProfileDir)
	fmt.Printf("Using Chromium remote debugging port %d\n", server.Config.BrowserDebugPort)
	if server.Config.BrowserBootstrap {
		fmt.Printf("Bootstrap browser opened at %s\n", server.Config.BrowserBootstrapURL)
	}
	return http.ListenAndServe(bindAddr, mux)
}

func ensureBrowserSession(ctx context.Context) error {
	if _, err := getDevToolsVersion(ctx); err == nil {
		return nil
	}

	if server.Config.BrowserBootstrap {
		if err := LaunchBrowserBootstrap(server.Config.BrowserBootstrapURL); err != nil {
			return fmt.Errorf("launch browser bootstrap: %w", err)
		}
	} else {
		if err := launchBackgroundBrowser(); err != nil {
			return fmt.Errorf("launch background browser: %w", err)
		}
	}

	return waitForBrowserSession(ctx, 15*time.Second)
}

func LaunchBrowserBootstrap(targetURL string) error {
	return launchBrowserProcess(targetURL, false)
}

func launchBackgroundBrowser() error {
	return launchBrowserProcess("about:blank", true)
}

func launchBrowserProcess(targetURL string, headless bool) error {
	if err := os.MkdirAll(filepath.Clean(server.Config.BrowserProfileDir), 0700); err != nil {
		return fmt.Errorf("mkdir browser profile dir: %w", err)
	}
	if strings.TrimSpace(targetURL) == "" {
		targetURL = "https://chaturbate.com/"
	}

	args := browserCommonArgs(server.Config.BrowserProfileDir)
	if headless {
		args = append(args, "--headless=new")
	} else {
		args = append(args, "--new-window")
	}
	args = append(args, targetURL)

	cmd := exec.Command(server.Config.BrowserPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if server.Config.Debug {
		label := "browser bootstrap"
		if headless {
			label = "browser background"
		}
		fmt.Printf("[DEBUG] %s: %s %s\n", label, server.Config.BrowserPath, strings.Join(args, " "))
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func browserCommonArgs(profileDir string) []string {
	return []string{
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--window-size=1280,720",
		fmt.Sprintf("--remote-debugging-port=%d", server.Config.BrowserDebugPort),
		"--user-data-dir=" + profileDir,
	}
}

func waitForBrowserSession(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := getDevToolsVersion(ctx); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("browser devtools endpoint did not become ready on port %d", server.Config.BrowserDebugPort)
}

func fetchHTMLViaDevTools(ctx context.Context, targetURL string) (string, error) {
	target, err := createDevToolsTarget(ctx, targetURL)
	if err != nil {
		return "", fmt.Errorf("create devtools target: %w", err)
	}
	defer closeDevToolsTarget(ctx, target.ID)

	client, err := newCDPClient(ctx, target.WebSocketDebuggerURL)
	if err != nil {
		return "", fmt.Errorf("connect devtools websocket: %w", err)
	}
	defer client.Close()

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}); err != nil {
		return "", fmt.Errorf("page enable: %w", err)
	}
	if _, err := client.Call(ctx, "Runtime.enable", map[string]any{}); err != nil {
		return "", fmt.Errorf("runtime enable: %w", err)
	}
	if _, err := client.Call(ctx, "Page.navigate", map[string]any{"url": targetURL}); err != nil {
		return "", fmt.Errorf("page navigate: %w", err)
	}

	time.Sleep(4 * time.Second)

	result, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "document.documentElement.outerHTML",
		"returnByValue": true,
	})
	if err != nil {
		return "", fmt.Errorf("runtime evaluate: %w", err)
	}

	var payload struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", fmt.Errorf("decode runtime evaluate: %w", err)
	}
	body := strings.TrimSpace(payload.Result.Value)
	if body == "" {
		return "", fmt.Errorf("devtools returned empty html")
	}

	locationResult, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "window.location.href",
		"returnByValue": true,
	})
	if err != nil {
		return "", fmt.Errorf("runtime evaluate location: %w", err)
	}
	var locationPayload struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(locationResult, &locationPayload); err != nil {
		return "", fmt.Errorf("decode runtime evaluate location: %w", err)
	}

	titleResult, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "document.title",
		"returnByValue": true,
	})
	if err != nil {
		return "", fmt.Errorf("runtime evaluate title: %w", err)
	}
	var titlePayload struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(titleResult, &titlePayload); err != nil {
		return "", fmt.Errorf("decode runtime evaluate title: %w", err)
	}

	if server.Config.Debug {
		fmt.Printf("[DEBUG] browser landed on %s title=%q bytes=%d\n", locationPayload.Result.Value, titlePayload.Result.Value, len(body))
	}

	resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/html"}}}
	if isCloudflareBlockPage(resp, []byte(body)) {
		if server.Config.Debug {
			dumpBrowserDebugResponse(targetURL, locationPayload.Result.Value, titlePayload.Result.Value, body)
		}
		return "", ErrCloudflareBlocked
	}
	return body, nil
}

func fetchAPIViaDevTools(ctx context.Context, pageURL, targetURL string) (*browserFetchResult, error) {
	target, err := createDevToolsTarget(ctx, pageURL)
	if err != nil {
		return nil, fmt.Errorf("create devtools target: %w", err)
	}
	defer closeDevToolsTarget(ctx, target.ID)

	client, err := newCDPClient(ctx, target.WebSocketDebuggerURL)
	if err != nil {
		return nil, fmt.Errorf("connect devtools websocket: %w", err)
	}
	defer client.Close()

	if _, err := client.Call(ctx, "Page.enable", map[string]any{}); err != nil {
		return nil, fmt.Errorf("page enable: %w", err)
	}
	if _, err := client.Call(ctx, "Runtime.enable", map[string]any{}); err != nil {
		return nil, fmt.Errorf("runtime enable: %w", err)
	}
	if _, err := client.Call(ctx, "Page.navigate", map[string]any{"url": pageURL}); err != nil {
		return nil, fmt.Errorf("page navigate: %w", err)
	}

	time.Sleep(4 * time.Second)

	expression := fmt.Sprintf(`(async () => {
		const resp = await fetch(%q, {
			credentials: "include",
			headers: {
				"Accept": "*/*",
				"Cache-Control": "no-cache",
				"Pragma": "no-cache",
				"X-Requested-With": "XMLHttpRequest"
			}
		});
		return {
			status: resp.status,
			final_url: resp.url,
			title: document.title,
			body: await resp.text()
		};
	})()`, targetURL)

	result, err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime evaluate api fetch: %w", err)
	}

	var payload struct {
		Result struct {
			Value browserFetchResult `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return nil, fmt.Errorf("decode runtime evaluate api fetch: %w", err)
	}
	out := &payload.Result.Value
	out.Body = strings.TrimSpace(out.Body)
	if out.Body == "" {
		return nil, fmt.Errorf("browser api fetch returned empty body")
	}
	if server.Config.Debug {
		fmt.Printf("[DEBUG] browser API response: page=%s target=%s status=%d final=%s title=%q bytes=%d\n", pageURL, targetURL, out.Status, out.FinalURL, out.Title, len(out.Body))
	}

	if isCloudflareBlockPage(&http.Response{Header: http.Header{"Content-Type": []string{"text/html"}}}, []byte(out.Body)) {
		if server.Config.Debug {
			dumpBrowserDebugResponse(targetURL, out.FinalURL, out.Title, out.Body)
		}
		return nil, ErrCloudflareBlocked
	}
	if out.Status >= 400 {
		return nil, fmt.Errorf("browser api fetch returned status %d", out.Status)
	}
	return out, nil
}

func dumpBrowserDebugResponse(requestURL, finalURL, title, body string) {
	tmpFile, err := os.CreateTemp("", "chaturbate-browser-debug-*.html")
	if err != nil {
		fmt.Printf("[DEBUG] failed to create browser debug temp file: %v\n", err)
		return
	}
	defer tmpFile.Close()

	if _, err := fmt.Fprintf(tmpFile, "<!-- request=%s final=%s title=%s -->\n%s", requestURL, finalURL, title, body); err != nil {
		fmt.Printf("[DEBUG] failed to write browser debug temp file: %v\n", err)
		return
	}
	fmt.Printf("[DEBUG] browser HTML written to: %s\n", tmpFile.Name())
}

func createDevToolsTarget(ctx context.Context, targetURL string) (*devToolsTarget, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", server.Config.BrowserDebugPort, url.QueryEscape(targetURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var target devToolsTarget
	if err := json.Unmarshal(raw, &target); err != nil {
		return nil, err
	}
	if target.ID == "" || target.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("devtools target missing fields")
	}
	return &target, nil
}

func closeDevToolsTarget(ctx context.Context, targetID string) {
	if targetID == "" {
		return
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/close/%s", server.Config.BrowserDebugPort, targetID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func getDevToolsVersion(ctx context.Context) (*devToolsVersion, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/version", server.Config.BrowserDebugPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var version devToolsVersion
	if err := json.NewDecoder(resp.Body).Decode(&version); err != nil {
		return nil, err
	}
	if version.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("browser websocket debugger URL missing")
	}
	return &version, nil
}

func newCDPClient(ctx context.Context, rawURL string) (*cdpClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n",
		path,
		u.Host,
		key,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}

	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" {
			break
		}
	}

	return &cdpClient{conn: conn, reader: reader}, nil
}

func (c *cdpClient) Close() error {
	return c.conn.Close()
}

func (c *cdpClient) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	payload, err := json.Marshal(map[string]any{
		"id":     id,
		"method": method,
		"params": params,
	})
	if err != nil {
		return nil, err
	}
	if err := c.writeTextFrame(payload); err != nil {
		return nil, err
	}

	for {
		msg, err := c.readTextFrame(ctx)
		if err != nil {
			return nil, err
		}

		var resp cdpResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf(resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *cdpClient) writeTextFrame(payload []byte) error {
	header := []byte{0x81}
	payloadLen := len(payload)
	switch {
	case payloadLen < 126:
		header = append(header, byte(0x80|payloadLen))
	case payloadLen <= 65535:
		header = append(header, 0x80|126, byte(payloadLen>>8), byte(payloadLen))
	default:
		header = append(header,
			0x80|127,
			byte(payloadLen>>56), byte(payloadLen>>48), byte(payloadLen>>40), byte(payloadLen>>32),
			byte(payloadLen>>24), byte(payloadLen>>16), byte(payloadLen>>8), byte(payloadLen),
		)
	}

	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)

	masked := make([]byte, payloadLen)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

func (c *cdpClient) readTextFrame(ctx context.Context) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	}

	first, err := c.reader.ReadByte()
	if err != nil {
		return nil, err
	}
	second, err := c.reader.ReadByte()
	if err != nil {
		return nil, err
	}

	opcode := first & 0x0f
	if opcode == 0x8 {
		return nil, io.EOF
	}

	payloadLen := int(second & 0x7f)
	switch payloadLen {
	case 126:
		var size [2]byte
		if _, err := io.ReadFull(c.reader, size[:]); err != nil {
			return nil, err
		}
		payloadLen = int(size[0])<<8 | int(size[1])
	case 127:
		var size [8]byte
		if _, err := io.ReadFull(c.reader, size[:]); err != nil {
			return nil, err
		}
		payloadLen = 0
		for _, b := range size {
			payloadLen = (payloadLen << 8) | int(b)
		}
	}

	if second&0x80 != 0 {
		var mask [4]byte
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return nil, err
		}
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
