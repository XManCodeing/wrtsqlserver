package onlineupdate

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Handler exposes host-only operations and strictly validated GitHub/GHCR source configuration.
func (service *Service) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		if request.URL.RawQuery != "" {
			respondError(writer, http.StatusBadRequest, "不支持查询参数")
			return
		}
		if service.handlePorts(writer, request) {
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/status" {
			writeJSON(writer, http.StatusOK, service.Status())
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/source" {
			writeJSON(writer, http.StatusOK, service.Source())
			return
		}
		if request.Method != http.MethodPost {
			respondError(writer, http.StatusMethodNotAllowed, "请求方法不支持")
			return
		}
		var err error
		switch request.URL.Path {
		case "/source":
			var source Source
			if err = decodeStrict(request.Body, 4096, &source); err == nil {
				source, err = service.SaveSource(source)
			}
			if err != nil {
				respondError(writer, http.StatusConflict, err.Error())
			} else {
				writeJSON(writer, http.StatusOK, source)
			}
			return
		case "/check":
			var body struct{}
			if request.ContentLength != 0 {
				err = decodeStrict(request.Body, 4096, &body)
			}
			if err == nil {
				err = service.Check()
			}
		case "/apply":
			var body struct {
				Version      string `json:"version"`
				Digest       string `json:"digest"`
				Confirmation string `json:"confirmation"`
			}
			if err = decodeStrict(request.Body, 4096, &body); err == nil {
				err = service.Apply(body.Version, body.Digest, body.Confirmation)
			}
		case "/rollback":
			var body struct {
				Confirmation string `json:"confirmation"`
			}
			if err = decodeStrict(request.Body, 4096, &body); err == nil {
				err = service.Rollback(body.Confirmation)
			}
		default:
			respondError(writer, http.StatusNotFound, "接口不存在")
			return
		}
		if err != nil {
			respondError(writer, http.StatusConflict, err.Error())
			return
		}
		writeJSON(writer, http.StatusAccepted, service.Status())
	})
}

// writeJSON emits only prevalidated status/error structures, with no diagnostic internals.
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// respondError returns a safe summary instead of raw subprocess or transport errors.
func respondError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

// PrepareHost verifies root-only state access and the exact socket directory ownership/mode.
func PrepareHost() error {
	if os.Geteuid() != 0 {
		return errors.New("relay-updater must run as host root")
	}
	for _, path := range []string{Root, filepath.Dir(SocketPath)} {
		mode, group := os.FileMode(0750), 10001
		if path == Root {
			mode, group = 0700, 0
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			if err = os.Mkdir(path, mode); err != nil {
				return errors.New("cannot create updater directory")
			}
			if err = os.Chown(path, 0, group); err != nil {
				return err
			}
			if err = os.Chmod(path, mode); err != nil {
				return err
			}
		}
		if err := trustedPath(path); err != nil {
			return errors.New("untrusted updater directory")
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode().Perm() != mode || stat.Gid != uint32(group) {
			return errors.New("updater root requires root:root 0700; run directory requires root:10001 0750")
		}
	}
	return nil
}

// ListenHost holds an exclusive process lock and binds only the fixed filesystem socket.
// The returned cleanup function closes the listener, removes the socket and releases the lock.
func ListenHost() (net.Listener, func(), error) {
	if err := PrepareHost(); err != nil {
		return nil, nil, err
	}
	lockPath := Root + "/process.lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (net.Listener, func(), error) { lock.Close(); return nil, nil, err }
	if _, err = privateRead(lockPath, 16); err != nil {
		return fail(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(errors.New("updater already running"))
	}
	if info, statErr := os.Lstat(SocketPath); statErr == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSocket == 0 || stat.Uid != 0 || stat.Gid != 10001 || info.Mode().Perm() != 0660 {
			return fail(errors.New("unsafe existing control socket"))
		}
		if err = os.Remove(SocketPath); err != nil {
			return fail(err)
		}
	} else if !os.IsNotExist(statErr) {
		return fail(statErr)
	}
	listener, err := net.Listen("unix", SocketPath)
	if err != nil {
		return fail(err)
	}
	if err = os.Chown(SocketPath, 0, 10001); err == nil {
		err = os.Chmod(SocketPath, 0660)
	}
	if err != nil {
		listener.Close()
		return fail(err)
	}
	cleanup := func() { listener.Close(); os.Remove(SocketPath); lock.Close() }
	return listener, cleanup, nil
}

// Wait blocks shutdown until the independent operation reaches a terminal state.
func (service *Service) Wait() {
	for {
		service.mu.Lock()
		busy := service.busy
		service.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
