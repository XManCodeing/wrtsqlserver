package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const onlineResponseLimit = 64 << 10

// onlineUpdateProxy only connects to the administrator-configured Unix socket; it never starts an updater.
type onlineUpdateProxy struct {
	client *http.Client
}

// newOnlineUpdateProxy accepts an absolute socket path and returns a bounded local client, or nil when disabled.
func newOnlineUpdateProxy(socket string) *onlineUpdateProxy {
	if socket == "" {
		return nil
	}
	if !filepath.IsAbs(socket) || strings.ContainsAny(socket, "\x00\r\n") {
		log.Print("online updates disabled: invalid Unix socket configuration")
		return nil
	}
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, MaxResponseHeaderBytes: 8 << 10,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socket)
		},
	}
	return &onlineUpdateProxy{client: &http.Client{
		Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// disabledOnlineStatus returns the complete status contract without probing or starting any process.
func disabledOnlineStatus() map[string]any {
	return map[string]any{"enabled": false, "currentVersion": version, "latest": nil,
		"state": "idle", "phase": "idle", "message": "在线更新未启用。", "operationId": "", "updatedAt": "",
		"canApply": false, "canRollback": false}
}

var onlineVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
var onlineDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

var onlineRepositoryPattern = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`)
var onlineImagePattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+$`)

// onlineSource contains only public repository and image identifiers, never credentials.
type onlineSource struct {
	RepositoryURL string `json:"repositoryUrl"`
	Image         string `json:"image"`
}

// validOnlineSource validates bounded GitHub/GHCR identifiers without fetching user-provided URLs.
func validOnlineSource(source onlineSource) bool {
	return len(source.RepositoryURL) <= 256 && len(source.Image) <= 256 &&
		onlineRepositoryPattern.MatchString(source.RepositoryURL) && onlineImagePattern.MatchString(source.Image) &&
		!strings.HasSuffix(source.RepositoryURL, "/.") && !strings.HasSuffix(source.RepositoryURL, "/..")
}

// onlineRequestBody validates the fixed operation payload; returns unchanged field values and validity.
func onlineRequestBody(writer http.ResponseWriter, request *http.Request, action string) ([]byte, bool) {
	var payload any
	switch action {
	case "/source":
		input := onlineSource{}
		if !decodeJSON(writer, request, &input) {
			return nil, false
		}
		if !validOnlineSource(input) {
			apiError(writer, 400, "请输入有效的 GitHub 仓库和 GHCR 镜像地址，每项不超过 256 字符。")
			return nil, false
		}
		payload = input
	case "/apply":
		input := struct {
			Version      string `json:"version"`
			Digest       string `json:"digest"`
			Confirmation string `json:"confirmation"`
		}{}
		if !decodeJSON(writer, request, &input) {
			return nil, false
		}
		if len(input.Version) > 128 || !onlineVersionPattern.MatchString(input.Version) || !onlineDigestPattern.MatchString(input.Digest) || input.Confirmation != "更新 "+input.Version {
			apiError(writer, 400, "更新版本、摘要或确认文字无效。")
			return nil, false
		}
		payload = input
	case "/rollback":
		input := struct {
			Confirmation string `json:"confirmation"`
		}{}
		if !decodeJSON(writer, request, &input) {
			return nil, false
		}
		if input.Confirmation != "回退转发程序" {
			apiError(writer, 400, "回退确认文字无效。")
			return nil, false
		}
		payload = input
	default:
		// Checking accepts no user-supplied URLs or options, including query strings.
		body, err := io.ReadAll(io.LimitReader(request.Body, 3))
		if err != nil || (len(body) != 0 && string(body) != "{}") {
			apiError(writer, 400, "检测更新不接受参数。")
			return nil, false
		}
		return nil, true
	}
	body, err := json.Marshal(payload)
	if err != nil {
		apiError(writer, 500, "无法准备更新请求。")
		return nil, false
	}
	return body, true
}

// handleOnlineUpdates routes fixed authenticated operations; ServeHTTP has already enforced session/origin/CSRF.
func (app *App) handleOnlineUpdates(writer http.ResponseWriter, request *http.Request) {
	action := strings.TrimPrefix(request.URL.Path, "/api/online-updates")
	method := http.MethodPost
	if action == "" {
		action, method = "/status", http.MethodGet
	}
	if action == "/source" && request.Method == http.MethodGet {
		method = http.MethodGet
	}
	if action != "/source" && action != "/status" && action != "/check" && action != "/apply" && action != "/rollback" || request.URL.Path == "/api/online-updates/status" {
		apiError(writer, 404, "API endpoint not found")
		return
	}
	if request.Method != method {
		apiError(writer, 405, "method not allowed")
		return
	}
	if request.URL.RawQuery != "" {
		apiError(writer, 400, "在线更新不接受查询参数。")
		return
	}
	if app.onlineUpdates == nil {
		if action == "/status" {
			writeJSON(writer, 200, disabledOnlineStatus())
		} else {
			apiError(writer, 503, "在线更新未启用。")
		}
		return
	}
	var body []byte
	if method == http.MethodPost {
		var valid bool
		body, valid = onlineRequestBody(writer, request, action)
		if !valid {
			return
		}
	}
	// Serialize acceptance with mapping commits; the host persists its busy state before 202.
	app.mu.Lock()
	defer app.mu.Unlock()
	app.onlineUpdates.forward(writer, request, action, body)
}

// forward sends a single request to a constant local host/path; success bytes and HTTP 202 remain unchanged.
func (proxy *onlineUpdateProxy) forward(writer http.ResponseWriter, request *http.Request, action string, body []byte) {
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, "http://relay-updater"+action, bytes.NewReader(body))
	if err != nil {
		apiError(writer, 500, "无法准备更新请求。")
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	response, err := proxy.client.Do(upstream)
	if err != nil {
		log.Print("online updater request unavailable; operation result may be pending")
		apiError(writer, 502, "暂时无法连接更新服务；操作结果未确认，请刷新状态，不要重复提交。")
		return
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, onlineResponseLimit+1))
	if err != nil || len(payload) > onlineResponseLimit || !json.Valid(payload) {
		log.Print("online updater response rejected")
		apiError(writer, 502, "更新服务响应无效；请刷新状态确认结果。")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := response.StatusCode
		if code < 400 || code >= 500 {
			code = 502
		}
		apiError(writer, code, "更新服务未接受请求；请刷新状态后核实版本、操作进度及更新服务配置。")
		return
	}
	if request.Method == http.MethodPost {
		log.Printf("online updater accepted operation %s (HTTP %d)", action, response.StatusCode)
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(response.StatusCode)
	if _, err = writer.Write(payload); err != nil {
		log.Print("online updater response write failed")
	}
}
