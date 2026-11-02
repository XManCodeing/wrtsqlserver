package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"time"
)

// portPanelStatus is the bounded public contract; config revision belongs to this application.
type portPanelStatus struct {
	Supported      bool   `json:"supported"`
	SchemaVersion  int    `json:"schemaVersion"`
	Revision       string `json:"revision"`
	ConfigRevision string `json:"configRevision"`
	DesiredPorts   []int  `json:"desiredPorts"`
	AppliedPorts   []int  `json:"appliedPorts"`
	ActualPorts    []int  `json:"actualPorts"`
	RulesReady     bool   `json:"rulesReady"`
	State          string `json:"state"`
	OperationID    string `json:"operationId"`
	Message        string `json:"message"`
	CanApply       bool   `json:"canApply"`
	RequiredPorts  []int  `json:"requiredPorts,omitempty"`
	NeedsFinalize  bool   `json:"needsFinalize"`
}

// portRevision fingerprints configuration without exposing credentials; caller holds app.mu.
func (app *App) portRevision() string {
	payload, _ := json.Marshal(app.cfg.Mappings)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// desiredPorts derives unique enabled LAN listeners; malformed publication intentions fail closed.
func desiredPorts(cfg Config) ([]int, error) {
	ports := []int{}
	seen := map[int]bool{}
	for _, mapping := range cfg.Mappings {
		if !mapping.Enabled {
			continue
		}
		if mapping.ListenHost != "0.0.0.0" {
			return nil, errors.New("对外发布的已启用映射必须监听 0.0.0.0，请先修改监听地址")
		}
		port := mapping.ListenPort
		if port < 1024 || port > 65535 || port == 6379 || port == 8080 || port == 18080 {
			return nil, errors.New("映射包含保留端口或不允许发布的端口")
		}
		if !seen[port] {
			ports = append(ports, port)
			seen[port] = true
		}
	}
	if len(ports) > 64 {
		return nil, errors.New("最多发布 64 个转发端口")
	}
	sort.Ints(ports)
	return ports, nil
}

// hostJSON performs one bounded call to a fixed host endpoint; never retries mutations.
func (proxy *onlineUpdateProxy) hostJSON(ctx context.Context, method, path string, body []byte, target any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://relay-updater"+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := proxy.client.Do(request)
	if err != nil {
		return 0, errors.New("无法连接宿主管理服务，请查询状态，不要重复提交")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, onlineResponseLimit+1))
	if err != nil || len(payload) > onlineResponseLimit {
		return response.StatusCode, errors.New("宿主响应读取失败或超出限制")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, errors.New("宿主拒绝操作，请刷新状态并核对事务与端口配置")
	}
	if err := json.Unmarshal(payload, target); err != nil {
		return response.StatusCode, errors.New("宿主管理服务响应无效")
	}
	return response.StatusCode, nil
}

// readPortStatus reads only host state and enriches it from a locked application snapshot.
func (app *App) readPortStatus(ctx context.Context) (portPanelStatus, error) {
	status := portPanelStatus{SchemaVersion: 1, State: "idle", AppliedPorts: []int{}, ActualPorts: []int{}, DesiredPorts: []int{}, Message: "需要手动升级或启动宿主管理服务"}
	if app.onlineUpdates != nil {
		code, err := app.onlineUpdates.hostJSON(ctx, http.MethodGet, "/ports", nil, &status)
		if err != nil && code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			return status, err
		}
		if code == http.StatusNotFound || code == http.StatusMethodNotAllowed {
			status.Supported = false
			status.CanApply = false
		}
	}
	if status.Supported && !validPortStatus(status) {
		return status, errors.New("宿主端口状态格式无效，已禁止变更")
	}
	status.ConfigRevision = app.portRevision()
	ports, err := publicationPorts(app.cfg, status.RequiredPorts)
	if err != nil {
		status.CanApply = false
		status.Message = err.Error()
	} else {
		status.DesiredPorts = ports
	}
	return status, nil
}

// handlePorts handles authenticated fixed routes; serialization protects the accepted revision.
func (app *App) handlePorts(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" {
		apiError(writer, 400, "端口管理不接受查询参数")
		return
	}
	switch {
	case request.URL.Path == "/api/ports" && request.Method == http.MethodGet:
		app.mu.Lock()
		status, err := app.readPortStatus(request.Context())
		app.mu.Unlock()
		if err != nil {
			apiError(writer, 502, err.Error())
			return
		}
		writeJSON(writer, 200, status)
	case request.URL.Path == "/api/ports/apply" && request.Method == http.MethodPost:
		app.applyPorts(writer, request)
	default:
		apiError(writer, 404, "API endpoint not found")
	}
}

// applyPorts derives publication intent server-side and sends a single confirmed transaction.
func (app *App) applyPorts(writer http.ResponseWriter, request *http.Request) {
	input := struct {
		Revision         string `json:"revision"`
		ConfigRevision   string `json:"configRevision"`
		Confirmation     string `json:"confirmation"`
		FinalizePrevious bool   `json:"finalizePrevious"`
	}{}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.Confirmation != "应用端口变更" || len(input.Revision) > 128 || len(input.ConfigRevision) != 64 {
		apiError(writer, 400, "端口变更确认或修订号无效")
		return
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if input.ConfigRevision != app.portRevision() {
		apiError(writer, 409, "映射配置已变化，请刷新后重新确认")
		return
	}
	status, err := app.readPortStatus(request.Context())
	if err != nil {
		apiError(writer, 502, err.Error())
		return
	}
	if !status.Supported || !status.CanApply {
		apiError(writer, 409, status.Message)
		return
	}
	if input.Revision != status.Revision {
		apiError(writer, 409, "宿主端口状态已变化，请刷新")
		return
	}
	if status.NeedsFinalize && !input.FinalizePrevious {
		apiError(writer, 409, "需明确确认结束旧更新回退窗口")
		return
	}
	ports, err := publicationPorts(app.cfg, status.RequiredPorts)
	if err != nil {
		apiError(writer, 400, err.Error())
		return
	}
	payload, err := json.Marshal(struct {
		Ports            []int  `json:"ports"`
		Revision         string `json:"revision"`
		Confirmation     string `json:"confirmation"`
		FinalizePrevious bool   `json:"finalizePrevious"`
	}{ports, input.Revision, input.Confirmation, input.FinalizePrevious})
	if err != nil {
		apiError(writer, 500, "无法准备端口请求")
		return
	}
	code, err := app.onlineUpdates.hostJSON(request.Context(), http.MethodPost, "/ports/apply", payload, &status)
	if err != nil {
		log.Print("port apply outcome unconfirmed; query host status")
		apiError(writer, 502, err.Error())
		return
	}
	status.ConfigRevision, status.DesiredPorts = app.portRevision(), ports
	log.Print("host accepted port publication operation")
	writeJSON(writer, code, status)
}

// hostMutationAllowed rejects configuration writes during host transactions or unknown host state.
// Caller holds app.mu. Legacy hosts use their update status to retain mutation serialization.
func (app *App) hostMutationAllowed() error {
	if app.onlineUpdates == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := portPanelStatus{}
	code, err := app.onlineUpdates.hostJSON(ctx, http.MethodGet, "/ports", nil, &status)
	legacy := code == http.StatusNotFound || code == http.StatusMethodNotAllowed
	if err != nil && !legacy {
		return err
	}
	if !legacy && (!status.Supported || !validPortStatus(status)) {
		return errors.New("无法确认宿主端口状态，暂时禁止修改配置")
	}
	if status.State == "applying" || status.State == "manual_recovery" {
		return errors.New("宿主端口事务进行中或需要恢复，暂时禁止修改映射与代理")
	}
	update := struct {
		State string `json:"state"`
	}{}
	_, err = app.onlineUpdates.hostJSON(ctx, http.MethodGet, "/status", nil, &update)
	if err != nil {
		return err
	}
	switch update.State {
	case "checking", "downloading", "pulling", "applying", "rolling_back", "manual_recovery":
		return errors.New("宿主更新事务进行中或需要恢复，暂时禁止修改配置")
	}
	return nil
}
