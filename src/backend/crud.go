package main

import "net/http"

// ProxyView intentionally excludes persisted secrets and indicates only their presence.
type ProxyView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"passwordSet"`
}

// proxyView builds a safe, stable API representation of endpoint.
func proxyView(endpoint Proxy) ProxyView {
	return ProxyView{ID: endpoint.ID, Name: endpoint.Name, Host: endpoint.Host, Port: endpoint.Port, Username: endpoint.Username, PasswordSet: endpoint.Password != ""}
}

// crud validates collection/item method combinations and serializes all configuration mutations.
func (app *App) crud(writer http.ResponseWriter, request *http.Request, resource, id string) {
	if request.Method == "GET" && id == "" {
		app.mu.Lock()
		defer app.mu.Unlock()
		if resource == "proxies" {
			result := []ProxyView{}
			for _, proxy := range app.cfg.Proxies {
				result = append(result, proxyView(proxy))
			}
			writeJSON(writer, 200, result)
		} else {
			result := []MappingView{}
			for _, mapping := range app.cfg.Mappings {
				result = append(result, app.runtimes[mapping.ID].view())
			}
			writeJSON(writer, 200, result)
		}
		return
	}
	if !((request.Method == "POST" && id == "") || ((request.Method == "PUT" || request.Method == "DELETE") && id != "")) {
		apiError(writer, 405, "method not allowed")
		return
	}
	if resource == "proxies" {
		app.changeProxy(writer, request, id)
	} else {
		app.changeMapping(writer, request, id)
	}
}

// changeProxy validates credentials, protects referenced records and commits a safe view.
func (app *App) changeProxy(writer http.ResponseWriter, request *http.Request, id string) {
	input := ProxyInput{}
	if request.Method != "DELETE" {
		if !decodeJSON(writer, request, &input) {
			return
		}
		if err := validateProxy(input); err != nil {
			apiError(writer, 400, err.Error())
			return
		}
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	candidate := cloneConfig(app.cfg)
	index := -1
	for position, proxy := range candidate.Proxies {
		if proxy.ID == id {
			index = position
		}
	}
	if id != "" && index < 0 {
		apiError(writer, 404, "proxy not found")
		return
	}
	if request.Method == "DELETE" {
		for _, mapping := range candidate.Mappings {
			if mapping.ProxyID == id {
				apiError(writer, 409, "proxy is referenced by a mapping")
				return
			}
		}
		candidate.Proxies = append(candidate.Proxies[:index], candidate.Proxies[index+1:]...)
		if err := app.commit(candidate); err != nil {
			apiError(writer, 500, err.Error())
			return
		}
		writeJSON(writer, 200, map[string]bool{"ok": true})
		return
	}
	if id == "" {
		if len(candidate.Proxies) >= 256 {
			apiError(writer, 409, "proxy limit reached")
			return
		}
		var err error
		id, err = randomToken()
		if err != nil {
			apiError(writer, 500, "ID generation failed")
			return
		}
	}
	proxy := Proxy{ID: id, Name: input.Name, Host: input.Host, Port: input.Port, Username: input.Username, Password: input.Password}
	if index >= 0 && input.Password == "" && !input.ClearPassword {
		proxy.Password = candidate.Proxies[index].Password
	}
	if input.ClearPassword {
		proxy.Password = ""
	}
	if err := validateProxyCredentials(proxy); err != nil {
		apiError(writer, 400, err.Error())
		return
	}
	if index < 0 {
		candidate.Proxies = append(candidate.Proxies, proxy)
	} else {
		candidate.Proxies[index] = proxy
	}
	if err := app.commit(candidate); err != nil {
		apiError(writer, 500, err.Error())
		return
	}
	writeJSON(writer, 200, proxyView(proxy))
}

// mappingInput excludes server-owned IDs and all runtime fields from writable JSON.
type mappingInput struct {
	Name           string   `json:"name"`
	ListenHost     string   `json:"listenHost"`
	ListenPort     int      `json:"listenPort"`
	TargetHost     string   `json:"targetHost"`
	TargetPort     int      `json:"targetPort"`
	ProxyID        string   `json:"proxyId"`
	Enabled        bool     `json:"enabled"`
	MaxConnections int      `json:"maxConnections"`
	AllowedCIDRs   []string `json:"allowedCIDRs"`
}

// changeMapping persists validated changes, then returns the actual listener state, including failures.
func (app *App) changeMapping(writer http.ResponseWriter, request *http.Request, id string) {
	input := mappingInput{}
	if request.Method != "DELETE" && !decodeJSON(writer, request, &input) {
		return
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	candidate := cloneConfig(app.cfg)
	index := -1
	for position, mapping := range candidate.Mappings {
		if mapping.ID == id {
			index = position
		}
	}
	if id != "" && index < 0 {
		apiError(writer, 404, "mapping not found")
		return
	}
	if request.Method == "DELETE" {
		candidate.Mappings = append(candidate.Mappings[:index], candidate.Mappings[index+1:]...)
		if err := app.commit(candidate); err != nil {
			apiError(writer, 500, err.Error())
			return
		}
		writeJSON(writer, 200, map[string]bool{"ok": true})
		return
	}
	if id == "" {
		if len(candidate.Mappings) >= 256 {
			apiError(writer, 409, "mapping limit reached")
			return
		}
		var err error
		id, err = randomToken()
		if err != nil {
			apiError(writer, 500, "ID generation failed")
			return
		}
	}
	mapping := Mapping{ID: id, Name: input.Name, ListenHost: input.ListenHost, ListenPort: input.ListenPort, TargetHost: input.TargetHost, TargetPort: input.TargetPort, ProxyID: input.ProxyID, Enabled: input.Enabled, MaxConnections: input.MaxConnections, AllowedCIDRs: input.AllowedCIDRs}
	if err := validateMapping(mapping, candidate, app.apiAddr); err != nil {
		apiError(writer, 400, err.Error())
		return
	}
	if index < 0 {
		candidate.Mappings = append(candidate.Mappings, mapping)
	} else {
		candidate.Mappings[index] = mapping
	}
	if err := app.commit(candidate); err != nil {
		apiError(writer, 500, err.Error())
		return
	}
	// Explicitly saving an errored mapping retries only that mapping's listener.
	if app.runtimes[id].view().Status == "error" {
		app.runtimes[id].Stop()
		app.runtimes[id] = newRuntime(mapping, proxyFor(app.cfg, mapping.ProxyID), app.slots, &app.active)
	}
	writeJSON(writer, 200, app.runtimes[id].view())
}
