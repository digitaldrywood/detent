package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"

	"github.com/digitaldrywood/detent/internal/web"
)

type demoCaptureBrowser struct {
	cancel      context.CancelFunc
	cmd         *exec.Cmd
	port        int
	output      *captureBuffer
	userDataDir string
}

type demoCapturePage struct {
	client *demoCaptureCDPClient
}

type demoCaptureCDPClient struct {
	conn    *websocket.Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan demoCaptureCDPResponse
}

type demoCaptureCDPResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *demoCaptureCDPError
}

type demoCaptureCDPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func startDemoCaptureBrowser(ctx context.Context, cfg demoCaptureConfig) (*demoCaptureBrowser, error) {
	browserPath, err := demoCaptureBrowserPath(cfg.BrowserPath)
	if err != nil {
		return nil, err
	}
	port, err := freeDemoCaptureTCPPort()
	if err != nil {
		return nil, err
	}
	userDataDir, err := os.MkdirTemp("", "detent-demo-capture-browser-*")
	if err != nil {
		return nil, fmt.Errorf("create browser user data directory: %w", err)
	}
	browserCtx, cancel := context.WithCancel(ctx)
	args := []string{
		"--headless=new",
		"--disable-background-networking",
		"--disable-default-apps",
		"--disable-dev-shm-usage",
		"--disable-gpu",
		"--disable-popup-blocking",
		"--no-default-browser-check",
		"--no-first-run",
		"--no-sandbox",
		"--remote-allow-origins=*",
		"--remote-debugging-address=127.0.0.1",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--window-size=%d,%d", cfg.Width, cfg.Height),
		"--user-data-dir=" + userDataDir,
		"about:blank",
	}
	cmd := exec.CommandContext(browserCtx, browserPath, args...) // #nosec G204 -- capture launches an explicit browser path with fixed headless CDP arguments.
	output := &captureBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		cancel()
		discardDemoCaptureError(os.RemoveAll(userDataDir))
		return nil, fmt.Errorf("start browser %s: %w", browserPath, err)
	}
	browser := &demoCaptureBrowser{cancel: cancel, cmd: cmd, port: port, output: output, userDataDir: userDataDir}
	if err := browser.waitForVersion(ctx, cfg.Timeout); err != nil {
		browser.Close()
		return nil, err
	}
	return browser, nil
}

func demoCaptureBrowserPath(explicit string) (string, error) {
	if path := strings.TrimSpace(explicit); path != "" {
		if executableDemoCaptureFile(path) {
			return path, nil
		}
		return "", fmt.Errorf("browser executable is not runnable: %s", path)
	}
	if path := strings.TrimSpace(os.Getenv("DETENT_CAPTURE_BROWSER")); path != "" {
		if executableDemoCaptureFile(path) {
			return path, nil
		}
		return "", fmt.Errorf("DETENT_CAPTURE_BROWSER is not runnable: %s", path)
	}
	for _, name := range demoCaptureBrowserNames() {
		if path, err := exec.LookPath(name); err == nil && executableDemoCaptureFile(path) {
			return path, nil
		}
	}
	for _, path := range demoCaptureBrowserPaths() {
		if executableDemoCaptureFile(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("chrome or chromium was not found; pass --browser or set DETENT_CAPTURE_BROWSER")
}

func demoCaptureBrowserNames() []string {
	switch goruntime.GOOS {
	case "windows":
		return []string{"chrome.exe", "msedge.exe"}
	case "darwin":
		return []string{"google-chrome", "chromium", "microsoft-edge"}
	default:
		return []string{"google-chrome-stable", "google-chrome", "chromium-browser", "chromium", "microsoft-edge"}
	}
}

func demoCaptureBrowserPaths() []string {
	switch goruntime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "windows":
		roots := []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")}
		relatives := []string{
			filepath.Join("Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join("Microsoft", "Edge", "Application", "msedge.exe"),
		}
		var paths []string
		for _, root := range roots {
			if strings.TrimSpace(root) == "" {
				continue
			}
			for _, relative := range relatives {
				paths = append(paths, filepath.Join(root, relative))
			}
		}
		return paths
	default:
		return nil
	}
}

func executableDemoCaptureFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func freeDemoCaptureTCPPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve browser debug port: %w", err)
	}
	defer func() {
		discardDemoCaptureError(listener.Close())
	}()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("browser debug listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	return addr.Port, nil
}

func (b *demoCaptureBrowser) waitForVersion(ctx context.Context, timeout time.Duration) error {
	client := http.Client{Timeout: 2 * time.Second}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	versionURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", b.port)
	var lastErr error
	for deadline.Err() == nil {
		req, err := http.NewRequestWithContext(deadline, http.MethodGet, versionURL, nil)
		if err != nil {
			return fmt.Errorf("create browser version request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			_, readErr := io.Copy(io.Discard, resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				return fmt.Errorf("read browser version response: %w", readErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close browser version response: %w", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for browser CDP endpoint: %w\n%s", lastErr, b.output.String())
}

func (b *demoCaptureBrowser) NewPage(ctx context.Context) (*demoCapturePage, error) {
	client := http.Client{Timeout: 10 * time.Second}
	newPageURL := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", b.port, url.QueryEscape("about:blank"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, newPageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create browser page request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create browser page: %w", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read browser page response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close browser page response: %w", closeErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("create browser page status = %s: %s", resp.Status, body)
	}
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &target); err != nil {
		return nil, fmt.Errorf("decode browser page response: %w", err)
	}
	if target.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("browser page response missing webSocketDebuggerUrl: %s", body)
	}
	cdp, err := newDemoCaptureCDPClient(target.WebSocketDebuggerURL)
	if err != nil {
		return nil, fmt.Errorf("connect browser page CDP: %w", err)
	}
	page := &demoCapturePage{client: cdp}
	if err := page.Call(ctx, "Page.enable", nil, nil); err != nil {
		page.Close()
		return nil, err
	}
	if err := page.Call(ctx, "Network.enable", nil, nil); err != nil {
		page.Close()
		return nil, err
	}
	return page, nil
}

func (b *demoCaptureBrowser) Close() {
	if b == nil {
		return
	}
	if b.cancel != nil {
		b.cancel()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		discardDemoCaptureError(b.cmd.Process.Kill())
		discardDemoCaptureError(b.cmd.Wait())
	}
	if b.userDataDir != "" {
		discardDemoCaptureError(os.RemoveAll(b.userDataDir))
	}
}

func (p *demoCapturePage) CaptureScenario(ctx context.Context, baseURL string, plan demoStillCapture, cfg demoCaptureConfig) error {
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o755); err != nil {
		return fmt.Errorf("create screenshot directory: %w", err)
	}
	pageCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := p.Call(pageCtx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             cfg.Width,
		"height":            cfg.Height,
		"deviceScaleFactor": cfg.DeviceScaleFactor,
		"mobile":            false,
	}, nil); err != nil {
		return fmt.Errorf("set browser viewport: %w", err)
	}
	headers := plan.Scenario.Headers
	if len(headers) == 0 {
		headers = map[string]string{web.DemoScenarioHeader: plan.Scenario.ID}
	}
	if err := p.Call(pageCtx, "Network.setExtraHTTPHeaders", map[string]any{"headers": headers}, nil); err != nil {
		return fmt.Errorf("set demo scenario headers: %w", err)
	}
	targetURL, err := demoCaptureScenarioURL(baseURL, plan.Scenario.Route)
	if err != nil {
		return err
	}
	if err := p.Call(pageCtx, "Page.navigate", map[string]any{"url": targetURL}, nil); err != nil {
		return fmt.Errorf("navigate to %s: %w", targetURL, err)
	}
	waitSelector := strings.TrimSpace(plan.Scenario.WaitSelector)
	if waitSelector == "" {
		waitSelector = "body"
	}
	if err := p.WaitForSelector(pageCtx, waitSelector); err != nil {
		return fmt.Errorf("wait for scenario %s selector %s: %w", plan.Scenario.ID, waitSelector, err)
	}
	if err := p.WaitForFonts(pageCtx); err != nil {
		return fmt.Errorf("wait for scenario %s fonts: %w", plan.Scenario.ID, err)
	}
	if err := p.Screenshot(pageCtx, plan.Path); err != nil {
		return fmt.Errorf("capture scenario %s screenshot: %w", plan.Scenario.ID, err)
	}
	return nil
}

func demoCaptureScenarioURL(baseURL string, route string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse demo runtime URL: %w", err)
	}
	ref, err := url.Parse(strings.TrimSpace(route))
	if err != nil {
		return "", fmt.Errorf("parse demo scenario route %q: %w", route, err)
	}
	return base.ResolveReference(ref).String(), nil
}

func (p *demoCapturePage) WaitForSelector(ctx context.Context, selector string) error {
	expression := fmt.Sprintf(`(() => {
		const element = document.querySelector(%s);
		if (document.readyState === "loading" || !element) {
			return false;
		}
		const style = window.getComputedStyle(element);
		if (!style || style.display === "none" || style.visibility === "hidden") {
			return false;
		}
		return Boolean(element.offsetWidth || element.offsetHeight || element.getClientRects().length);
	})()`, strconv.Quote(selector))
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		ok, err := p.EvalBool(ctx, expression)
		if err == nil && ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w: browser selector evaluation failed: %w", ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *demoCapturePage) WaitForFonts(ctx context.Context) error {
	var ok bool
	return p.Eval(ctx, `document.fonts ? document.fonts.ready.then(() => true) : true`, &ok)
}

func (p *demoCapturePage) Screenshot(ctx context.Context, path string) error {
	var result struct {
		Data string `json:"data"`
	}
	if err := p.Call(ctx, "Page.captureScreenshot", map[string]any{
		"format":                "png",
		"captureBeyondViewport": false,
		"fromSurface":           true,
	}, &result); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return fmt.Errorf("decode browser screenshot: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write browser screenshot %s: %w", path, err)
	}
	return nil
}

func (p *demoCapturePage) EvalBool(ctx context.Context, expression string) (bool, error) {
	var got bool
	if err := p.Eval(ctx, expression, &got); err != nil {
		return false, err
	}
	return got, nil
}

func (p *demoCapturePage) Eval(ctx context.Context, expression string, out any) error {
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := p.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"awaitPromise":  true,
		"returnByValue": true,
	}, &result); err != nil {
		return err
	}
	if len(result.ExceptionDetails) > 0 {
		return fmt.Errorf("runtime.evaluate exception for %s: %s", expression, result.ExceptionDetails)
	}
	if len(result.Result.Value) == 0 {
		return fmt.Errorf("runtime.evaluate returned no value for %s", expression)
	}
	if err := json.Unmarshal(result.Result.Value, out); err != nil {
		return fmt.Errorf("decode Runtime.evaluate value for %s: %w", expression, err)
	}
	return nil
}

func (p *demoCapturePage) Call(ctx context.Context, method string, params any, out any) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("browser page is closed")
	}
	if err := p.client.Call(ctx, method, params, out); err != nil {
		return fmt.Errorf("CDP %s: %w", method, err)
	}
	return nil
}

func (p *demoCapturePage) Close() {
	if p == nil || p.client == nil {
		return
	}
	p.client.Close()
}

func newDemoCaptureCDPClient(wsURL string) (*demoCaptureCDPClient, error) {
	conn, err := websocket.Dial(wsURL, "", "http://127.0.0.1/")
	if err != nil {
		return nil, err
	}
	client := &demoCaptureCDPClient{
		conn:    conn,
		pending: make(map[int64]chan demoCaptureCDPResponse),
	}
	go client.readLoop()
	return client, nil
}

func (c *demoCaptureCDPClient) Call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	response := make(chan demoCaptureCDPResponse, 1)
	c.mu.Lock()
	c.pending[id] = response
	c.mu.Unlock()

	payload := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		payload["params"] = params
	}
	if err := websocket.JSON.Send(c.conn, payload); err != nil {
		c.deletePending(id)
		return err
	}

	select {
	case resp := <-response:
		if resp.Error != nil {
			return fmt.Errorf("%s (%d)", resp.Error.Message, resp.Error.Code)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return err
			}
		}
		return nil
	case <-ctx.Done():
		c.deletePending(id)
		return ctx.Err()
	}
}

func (c *demoCaptureCDPClient) deletePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *demoCaptureCDPClient) readLoop() {
	for {
		var raw json.RawMessage
		if err := websocket.JSON.Receive(c.conn, &raw); err != nil {
			c.failPending(err)
			return
		}
		var envelope struct {
			ID     int64                `json:"id"`
			Method string               `json:"method"`
			Result json.RawMessage      `json:"result"`
			Error  *demoCaptureCDPError `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}
		if envelope.ID == 0 {
			continue
		}
		c.mu.Lock()
		pending := c.pending[envelope.ID]
		delete(c.pending, envelope.ID)
		c.mu.Unlock()
		if pending != nil {
			pending <- demoCaptureCDPResponse{ID: envelope.ID, Result: envelope.Result, Error: envelope.Error}
		}
	}
}

func (c *demoCaptureCDPClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, pending := range c.pending {
		delete(c.pending, id)
		pending <- demoCaptureCDPResponse{ID: id, Error: &demoCaptureCDPError{Message: err.Error()}}
	}
}

func (c *demoCaptureCDPClient) Close() {
	if c == nil || c.conn == nil {
		return
	}
	discardDemoCaptureError(c.conn.Close())
}
