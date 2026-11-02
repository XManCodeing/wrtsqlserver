package onlineupdate

import "net/http"

// handlePorts returns true for capability/port routes and never changes legacy status JSON.
func (service *Service) handlePorts(writer http.ResponseWriter, request *http.Request) bool {
	switch request.URL.Path {
	case "/capabilities":
		if request.Method != http.MethodGet {
			respondError(writer, 405, "请求方法不支持")
			return true
		}
		version := 0
		if service.portStore != nil {
			version = 1
		}
		writeJSON(writer, 200, map[string]int{"portManagement": version})
		return true
	case "/ports":
		if request.Method != http.MethodGet {
			respondError(writer, 405, "请求方法不支持")
			return true
		}
		if service.portStore == nil {
			respondError(writer, 404, "端口能力未启用")
			return true
		}
		writeJSON(writer, 200, service.Ports())
		return true
	case "/ports/apply":
		if request.Method != http.MethodPost {
			respondError(writer, 405, "请求方法不支持")
			return true
		}
		var body PortRequest
		err := decodeStrict(request.Body, 4096, &body)
		if err == nil {
			err = service.ApplyPorts(body)
		}
		if err != nil {
			respondError(writer, 409, err.Error())
		} else {
			writeJSON(writer, 202, service.Ports())
		}
		return true
	}
	return false
}
