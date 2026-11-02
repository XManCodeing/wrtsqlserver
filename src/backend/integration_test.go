package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// createMappingAPI submits a mapping and asserts the stable mapping view contract.
func createMappingAPI(t *testing.T, app *App, port int, proxyID string) MappingView {
	t.Helper()
	cookie, csrf := loginTest(t, app)
	input := mappingInput{Name: "DB", ListenHost: "127.0.0.1", ListenPort: port, TargetHost: "remote.db", TargetPort: 5432, ProxyID: proxyID, Enabled: true, MaxConnections: 10, AllowedCIDRs: []string{"127.0.0.1/32"}}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := requestAPI(app, "POST", "/api/mappings", string(body), cookie, csrf)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var view MappingView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// TestMappingErrorAndPersistenceRollback validates explicit binding errors, reference protection and failed-save rollback.
func TestMappingErrorAndPersistenceRollback(t *testing.T) {
	app := testApp(t)
	proxy, _ := fakeSOCKS(t, "", "")
	app.cfg.Proxies = []Proxy{proxy}
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	view := createMappingAPI(t, app, occupied.Addr().(*net.TCPAddr).Port, proxy.ID)
	if view.Status != "error" || view.LastError == "" {
		t.Fatal("bind failure not explicit", view)
	}
	cookie, csrf := loginTest(t, app)
	response := requestAPI(app, "DELETE", "/api/proxies/"+proxy.ID, "", cookie, csrf)
	if response.Code != 409 {
		t.Fatal("referenced proxy deleted", response.Code)
	}
	original := app.store.directory
	app.store.directory = filepath.Join(original, "nonexistent")
	response = requestAPI(app, "DELETE", "/api/mappings/"+view.ID, "", cookie, csrf)
	if response.Code != 500 || len(app.cfg.Mappings) != 1 || app.runtimes[view.ID] == nil {
		t.Fatal("failed save changed memory")
	}
	app.store.directory = original
	response = requestAPI(app, "DELETE", "/api/mappings/"+view.ID, "", cookie, csrf)
	if response.Code != 200 || len(app.runtimes) != 0 {
		t.Fatal("delete failed", response.Body.String())
	}
}

// TestDisableMappingIsolation proves changing one mapping interrupts its client but preserves another tunnel.
func TestDisableMappingIsolation(t *testing.T) {
	app := testApp(t)
	proxy, _ := fakeSOCKS(t, "", "")
	app.cfg.Proxies = []Proxy{proxy}
	// Reserve two temporary addresses and release them immediately before binding the application.
	ports := []int{}
	for index := 0; index < 2; index++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
		listener.Close()
	}
	first := createMappingAPI(t, app, ports[0], proxy.ID)
	second := createMappingAPI(t, app, ports[1], proxy.ID)
	firstClient := connectRuntime(t, app.runtimes[first.ID])
	secondClient := connectRuntime(t, app.runtimes[second.ID])
	assertEcho(t, firstClient)
	assertEcho(t, secondClient)
	cookie, csrf := loginTest(t, app)
	input := mappingInput{Name: first.Name, ListenHost: first.ListenHost, ListenPort: first.ListenPort, TargetHost: first.TargetHost, TargetPort: first.TargetPort, ProxyID: first.ProxyID, Enabled: false, MaxConnections: first.MaxConnections, AllowedCIDRs: first.AllowedCIDRs}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := requestAPI(app, "PUT", "/api/mappings/"+first.ID, string(body), cookie, csrf)
	if response.Code != 200 || app.runtimes[first.ID].view().Status != "stopped" {
		t.Fatal(response.Body.String())
	}
	if _, err := firstClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("disabled stream survived")
	}
	assertEcho(t, secondClient)
	if app.active.Load() != 1 {
		t.Fatal("unexpected active count", app.active.Load())
	}
	response = requestAPI(app, "POST", "/api/mappings/"+second.ID+"/test", "", cookie, csrf)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"ok":true`) {
		t.Fatal(response.Body.String())
	}
}

// TestStaticFallbackAndAPIIsolation ensures unknown API routes never receive the SPA HTML.
func TestStaticFallbackAndAPIIsolation(t *testing.T) {
	app := testApp(t)
	app.webDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(app.webDir, "index.html"), []byte("<html>SPA</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.webDir, "bundle.js"), []byte("asset"), 0600); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/dashboard": "<html>SPA</html>", "/bundle.js": "asset"} {
		response := requestAPI(app, "GET", path, "", nil, "")
		if response.Code != 200 || response.Body.String() != want {
			t.Fatal(path, response.Code, response.Body.String())
		}
	}
	cookie, _ := loginTest(t, app)
	response := requestAPI(app, "GET", "/api/unknown", "", cookie, "")
	if response.Code != 404 || strings.Contains(response.Body.String(), "SPA") {
		t.Fatal(response.Body.String())
	}
}

// TestAdminPasswordConfiguration checks unset/short secrets, file precedence and trailing newlines.
func TestAdminPasswordConfiguration(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD_FILE", "")
	t.Setenv("ADMIN_PASSWORD", "")
	if _, err := adminPassword(); err == nil {
		t.Fatal("empty admin password accepted")
	}
	t.Setenv("ADMIN_PASSWORD", "short")
	if _, err := adminPassword(); err == nil {
		t.Fatal("short password accepted")
	}
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("long-admin-password\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PASSWORD_FILE", path)
	password, err := adminPassword()
	if err != nil || password != "long-admin-password" {
		t.Fatal("file secret failed", err)
	}
}

// TestSourceSizeConstraints enforces the project's per-file and per-function line limits with the Go AST.
func TestSourceSizeConstraints(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(content), "\n") > 1000 {
			t.Error(path, "exceeds 1000 lines")
		}
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, path, content, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			declaration, ok := node.(*ast.FuncDecl)
			if !ok {
				return true
			}
			lines := positions.Position(declaration.End()).Line - positions.Position(declaration.Pos()).Line + 1
			if lines >= 100 {
				t.Error(path, declaration.Name.Name, "has", strconv.Itoa(lines), "lines; expected <100")
			}
			return true
		})
	}
}
