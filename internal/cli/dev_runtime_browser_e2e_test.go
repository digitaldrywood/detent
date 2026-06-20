package cli

import (
	"context"
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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/digitaldrywood/detent/internal/devruntime"
)

func TestStartKanbanDemoBrowserDragDrop(t *testing.T) {
	chromePath := chromeExecutable()
	if chromePath == "" {
		t.Skip("Chrome-family browser not found; set DETENT_BROWSER_E2E_CHROME to run browser drag/drop e2e")
	}

	const projectID = "demo-project"
	runtimeURL, done, cancel := startKanbanBrowserRuntime(t, projectID)
	defer cancel()

	browser := startCDPBrowser(t, chromePath)
	defer browser.Close()

	page := browser.NewPage(t, runtimeURL+"/projects/"+projectID+"/kanban")
	defer page.Close()
	page.WaitForExpression(t, `Boolean(window.htmx && window.__detentKanbanDragHandlersRegistered === true && document.querySelector("#project-kanban [data-kanban-card][data-kanban-issue-id='kanban-demo-backlog'][draggable='true']") && document.querySelector("[data-kanban-drop-state='Merging']"))`)
	page.Screenshot(t, "kanban-drag-before.png")
	page.SetLaneVisible(t, "Merging")
	page.Screenshot(t, "kanban-drag-target-visible.png")

	initialPosts := page.RequestCount(http.MethodPost, "/api/v1/kanban/move")
	if page.DragCardToLane(t, "kanban-demo-backlog", "Merging") {
		t.Fatal("Backlog to Merging drag/drop was accepted, want blocked")
	}
	page.WaitForExpression(t, `document.getElementById("kanban-feedback") && document.getElementById("kanban-feedback").textContent.includes("Move blocked by transition policy.")`)
	page.Screenshot(t, "kanban-drag-blocked.png")
	if got := page.RequestCount(http.MethodPost, "/api/v1/kanban/move"); got != initialPosts {
		t.Fatalf("blocked drag/drop POST count = %d, want %d", got, initialPosts)
	}
	assertKanbanCardCount(t, page, "Backlog", "kanban-demo-backlog", 1)
	assertKanbanCardCount(t, page, "Merging", "kanban-demo-backlog", 0)

	page.SetLaneVisible(t, "Todo")
	if !page.DragCardToLane(t, "kanban-demo-backlog", "Todo") {
		t.Fatal("Backlog to Todo drag/drop was rejected, want accepted")
	}
	page.WaitForRequestCount(t, http.MethodPost, "/api/v1/kanban/move", initialPosts+1)
	page.WaitForExpression(t, `document.getElementById("kanban-feedback") && document.getElementById("kanban-feedback").textContent.includes("Moved card to Todo.")`)
	page.WaitForExpression(t, `(() => {
		const todo = document.querySelector("[data-kanban-drop-state='Todo']");
		const backlog = document.querySelector("[data-kanban-drop-state='Backlog']");
		return todo && backlog &&
			todo.querySelectorAll("[data-kanban-issue-id='kanban-demo-backlog']").length === 1 &&
			backlog.querySelectorAll("[data-kanban-issue-id='kanban-demo-backlog']").length === 0;
	})()`)
	page.Screenshot(t, "kanban-drag-after.png")
	if page.EvalBool(t, `(() => {
		const dialog = document.getElementById("kanban-action-dialog");
		const content = document.getElementById("kanban-dialog-content");
		return Boolean((dialog && dialog.dataset.tuiDialogOpen === "true") || (content && content.textContent.trim() !== ""));
	})()`) {
		t.Fatal("kanban action dialog opened after direct drag/drop move")
	}

	page.ClickMoveButtonForCard(t, "kanban-demo-backlog")
	page.WaitForExpression(t, `(() => {
		const dialog = document.getElementById("kanban-action-dialog");
		const content = document.getElementById("kanban-dialog-content");
		return Boolean(dialog && dialog.dataset.tuiDialogOpen === "true" && content && content.textContent.includes("Move card"));
	})()`)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("startRunning() error = %v, want %v or %v", err, context.Canceled, context.DeadlineExceeded)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

func startKanbanBrowserRuntime(t *testing.T, projectID string) (string, chan error, context.CancelFunc) {
	t.Helper()

	isolatedRuntime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0, Demo: devruntime.DemoKanban, DemoProjectID: projectID})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(isolatedRuntime, "127.0.0.1", defaultOptions(), output))
	}()

	runtimeURL := waitForIsolatedRuntimeURL(t, output, done)
	waitForDashboard(t, runtimeURL+"/health", done)
	postRuntimeRefresh(t, runtimeURL, done)
	waitForDashboardCondition(t, runtimeURL+"/projects/"+projectID+"/kanban", done, "browser kanban board", func(body string) bool {
		return strings.Contains(body, `data-kanban-issue-id="kanban-demo-backlog"`) &&
			strings.Contains(body, `data-kanban-drop-state="Merging"`)
	})
	return runtimeURL, done, cancel
}

func assertKanbanCardCount(t *testing.T, page *cdpPage, lane string, issueID string, want int) {
	t.Helper()

	laneSelector := fmt.Sprintf("[data-kanban-drop-state=%q]", lane)
	cardSelector := fmt.Sprintf("[data-kanban-card][data-kanban-issue-id=%q]", issueID)
	got := page.EvalInt(t, fmt.Sprintf(`(() => {
		const lane = document.querySelector(%s);
		return lane ? lane.querySelectorAll(%s).length : -1;
	})()`, strconv.Quote(laneSelector), strconv.Quote(cardSelector)))
	if got != want {
		t.Fatalf("%s card count for %s = %d, want %d", lane, issueID, got, want)
	}
}

func chromeExecutable() string {
	if path := strings.TrimSpace(os.Getenv("DETENT_BROWSER_E2E_CHROME")); path != "" && executableFile(path) {
		return path
	}

	for _, name := range chromeExecutableNames() {
		if path, err := exec.LookPath(name); err == nil && executableFile(path) {
			return path
		}
	}
	for _, path := range chromeExecutablePaths() {
		if executableFile(path) {
			return path
		}
	}
	return ""
}

func chromeExecutableNames() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"chrome.exe", "msedge.exe"}
	case "darwin":
		return []string{"google-chrome", "chromium", "microsoft-edge"}
	default:
		return []string{"google-chrome-stable", "google-chrome", "chromium-browser", "chromium", "microsoft-edge"}
	}
}

func chromeExecutablePaths() []string {
	switch runtime.GOOS {
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

func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

type cdpBrowser struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
	port   int
}

func startCDPBrowser(t *testing.T, browserPath string) *cdpBrowser {
	t.Helper()

	port := freeTCPPort(t)
	userDataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
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
		"--window-size=3840,1200",
		"--user-data-dir=" + userDataDir,
		"about:blank",
	}
	cmd := exec.CommandContext(ctx, browserPath, args...)
	var output lockedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start browser %s: %v", browserPath, err)
	}

	browser := &cdpBrowser{cancel: cancel, cmd: cmd, port: port}
	t.Cleanup(browser.Close)
	browser.waitForVersion(t, &output)
	return browser
}

func freeTCPPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve browser debug port: %v", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("browser debug listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	return addr.Port
}

func (b *cdpBrowser) waitForVersion(t *testing.T, output *lockedBuffer) {
	t.Helper()

	client := http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	versionURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", b.port)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, versionURL, nil)
		if err != nil {
			cancel()
			t.Fatalf("create browser version request: %v", err)
		}
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			_, readErr := io.Copy(io.Discard, resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read browser version response: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("close browser version response: %v", closeErr)
			}
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for browser CDP endpoint: %v\n%s", lastErr, output.String())
}

func (b *cdpBrowser) NewPage(t *testing.T, pageURL string) *cdpPage {
	t.Helper()

	client := http.Client{Timeout: 10 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	newPageURL := fmt.Sprintf("http://127.0.0.1:%d/json/new?%s", b.port, url.QueryEscape(pageURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, newPageURL, nil)
	if err != nil {
		t.Fatalf("create browser page request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create browser page: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read browser page response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close browser page response: %v", closeErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create browser page status = %s: %s", resp.Status, body)
	}
	var target struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &target); err != nil {
		t.Fatalf("decode browser page response: %v", err)
	}
	if target.WebSocketDebuggerURL == "" {
		t.Fatalf("browser page response missing webSocketDebuggerUrl: %s", body)
	}

	cdp, err := newCDPClient(target.WebSocketDebuggerURL)
	if err != nil {
		t.Fatalf("connect browser page CDP: %v", err)
	}
	page := &cdpPage{client: cdp}
	page.startEvents()
	page.Call(t, "Page.enable", nil, nil)
	page.Call(t, "Network.enable", nil, nil)
	page.Call(t, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             3840,
		"height":            1200,
		"deviceScaleFactor": 1,
		"mobile":            false,
	}, nil)
	page.Call(t, "Page.navigate", map[string]any{"url": pageURL}, nil)
	return page
}

func (b *cdpBrowser) Close() {
	if b == nil {
		return
	}
	b.cancel()
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_ = b.cmd.Wait()
	}
}

type cdpPage struct {
	client *cdpClient
	mu     sync.Mutex
	reqs   []networkRequest
}

type networkRequest struct {
	Method string
	URL    string
}

func (p *cdpPage) startEvents() {
	go func() {
		for event := range p.client.Events() {
			if event.Method != "Network.requestWillBeSent" {
				continue
			}
			var params struct {
				Request struct {
					Method string `json:"method"`
					URL    string `json:"url"`
				} `json:"request"`
			}
			if err := json.Unmarshal(event.Params, &params); err != nil {
				continue
			}
			p.mu.Lock()
			p.reqs = append(p.reqs, networkRequest{Method: params.Request.Method, URL: params.Request.URL})
			p.mu.Unlock()
		}
	}()
}

func (p *cdpPage) Close() {
	if p == nil || p.client == nil {
		return
	}
	p.client.Close()
}

func (p *cdpPage) RequestCount(method string, path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0
	for _, req := range p.reqs {
		if req.Method != method {
			continue
		}
		parsed, err := url.Parse(req.URL)
		if err == nil && parsed.Path == path {
			count++
		}
	}
	return count
}

func (p *cdpPage) WaitForRequestCount(t *testing.T, method string, path string, want int) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if p.RequestCount(method, path) >= want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s %s request count = %d, want at least %d", method, path, p.RequestCount(method, path), want)
}

func (p *cdpPage) Call(t *testing.T, method string, params any, out any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.client.Call(ctx, method, params, out); err != nil {
		t.Fatalf("CDP %s: %v", method, err)
	}
}

func (p *cdpPage) EvalBool(t *testing.T, expression string) bool {
	t.Helper()

	var got bool
	p.Eval(t, expression, &got)
	return got
}

func (p *cdpPage) EvalInt(t *testing.T, expression string) int {
	t.Helper()

	var got int
	p.Eval(t, expression, &got)
	return got
}

func (p *cdpPage) Eval(t *testing.T, expression string, out any) {
	t.Helper()

	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	p.Call(t, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"awaitPromise":  true,
		"returnByValue": true,
	}, &result)
	if len(result.ExceptionDetails) > 0 {
		t.Fatalf("Runtime.evaluate exception for %s: %s", expression, result.ExceptionDetails)
	}
	if len(result.Result.Value) == 0 {
		t.Fatalf("Runtime.evaluate returned no value for %s", expression)
	}
	if err := json.Unmarshal(result.Result.Value, out); err != nil {
		t.Fatalf("decode Runtime.evaluate value for %s: %v\n%s", expression, err, result.Result.Value)
	}
}

func (p *cdpPage) WaitForExpression(t *testing.T, expression string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if p.EvalBool(t, expression) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for browser expression: %s", expression)
}

func (p *cdpPage) DragCardToLane(t *testing.T, issueID string, targetState string) bool {
	t.Helper()

	cardSelector := fmt.Sprintf("[data-kanban-issue-id=%q]", issueID)
	selector := fmt.Sprintf("[data-kanban-drop-state=%q]", targetState)
	return p.EvalBool(t, fmt.Sprintf(`(() => {
		const card = document.querySelector(%s);
		const lane = document.querySelector(%s);
		if (!card) {
			throw new Error("missing card " + %s);
		}
		if (!lane) {
			throw new Error("missing lane " + %s);
		}
		card.scrollIntoView({ block: "center", inline: "center" });
		lane.scrollIntoView({ block: "center", inline: "center" });
		const cardRect = card.getBoundingClientRect();
		const laneRect = lane.getBoundingClientRect();
		const data = new DataTransfer();
		data.effectAllowed = "move";
		data.setData("text/plain", %s);
		function fire(type, target, rect) {
			const event = new DragEvent(type, {
				bubbles: true,
				cancelable: true,
				dataTransfer: data,
				clientX: rect.left + Math.min(rect.width / 2, 160),
				clientY: rect.top + Math.min(rect.height / 2, 160),
			});
			target.dispatchEvent(event);
			return event.defaultPrevented;
		}
		fire("dragstart", card, cardRect);
		const accepted = fire("dragover", lane, laneRect);
		if (accepted) {
			fire("drop", lane, laneRect);
		}
		fire("dragend", card, cardRect);
		return accepted;
	})()`, strconv.Quote(cardSelector), strconv.Quote(selector), strconv.Quote(issueID), strconv.Quote(targetState), strconv.Quote(issueID)))
}

func (p *cdpPage) ClickMoveButtonForCard(t *testing.T, issueID string) {
	t.Helper()

	selector := fmt.Sprintf("[data-kanban-card][data-kanban-issue-id=%q] [aria-label^='Move ']", issueID)
	if !p.EvalBool(t, fmt.Sprintf(`(() => {
		const button = document.querySelector(%s);
		if (!button) {
			throw new Error("missing move button " + %s);
		}
		button.scrollIntoView({ block: "center", inline: "center" });
		button.click();
		return true;
	})()`, strconv.Quote(selector), strconv.Quote(selector))) {
		t.Fatalf("move button for %s was not clicked", issueID)
	}
}

func (p *cdpPage) SetLaneVisible(t *testing.T, targetState string) {
	t.Helper()

	selector := fmt.Sprintf("[data-kanban-drop-state=%q]", targetState)
	if !p.EvalBool(t, fmt.Sprintf(`(() => {
		const lane = document.querySelector(%s);
		if (!lane) {
			throw new Error("missing lane " + %s);
		}
		lane.dataset.projectKanbanLaneVisible = "true";
		return getComputedStyle(lane).display !== "none";
	})()`, strconv.Quote(selector), strconv.Quote(targetState))) {
		t.Fatalf("lane %s did not become visible", targetState)
	}
}

func (p *cdpPage) Screenshot(t *testing.T, name string) {
	t.Helper()

	var result struct {
		Data string `json:"data"`
	}
	p.Call(t, "Page.captureScreenshot", map[string]any{
		"format":                "png",
		"captureBeyondViewport": false,
	}, &result)
	raw, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		t.Fatalf("decode browser screenshot: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve test working directory: %v", err)
	}
	dir := filepath.Join(wd, "..", "..", "tmp", "browser-e2e")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create browser artifact directory: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write browser screenshot %s: %v", path, err)
	}
	t.Logf("wrote browser screenshot %s", path)
}

type cdpClient struct {
	conn    *websocket.Conn
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan cdpResponse
	events  chan cdpEvent
}

type cdpResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *cdpError       `json:"error"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cdpEvent struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func newCDPClient(wsURL string) (*cdpClient, error) {
	conn, err := websocket.Dial(wsURL, "", "http://127.0.0.1/")
	if err != nil {
		return nil, err
	}
	client := &cdpClient{
		conn:    conn,
		pending: make(map[int64]chan cdpResponse),
		events:  make(chan cdpEvent, 1024),
	}
	go client.readLoop()
	return client, nil
}

func (c *cdpClient) Events() <-chan cdpEvent {
	return c.events
}

func (c *cdpClient) Call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	response := make(chan cdpResponse, 1)

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

func (c *cdpClient) deletePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *cdpClient) readLoop() {
	defer close(c.events)
	for {
		var raw json.RawMessage
		if err := websocket.JSON.Receive(c.conn, &raw); err != nil {
			c.failPending(err)
			return
		}
		var envelope struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}
		if envelope.ID > 0 {
			c.mu.Lock()
			pending := c.pending[envelope.ID]
			delete(c.pending, envelope.ID)
			c.mu.Unlock()
			if pending != nil {
				pending <- cdpResponse{ID: envelope.ID, Result: envelope.Result, Error: envelope.Error}
			}
			continue
		}
		if envelope.Method != "" {
			select {
			case c.events <- cdpEvent{Method: envelope.Method, Params: envelope.Params}:
			default:
			}
		}
	}
}

func (c *cdpClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, pending := range c.pending {
		delete(c.pending, id)
		pending <- cdpResponse{ID: id, Error: &cdpError{Message: err.Error()}}
	}
}

func (c *cdpClient) Close() {
	if c == nil || c.conn == nil {
		return
	}
	_ = c.conn.Close()
}
