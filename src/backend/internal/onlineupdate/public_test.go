package onlineupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestOptionalToken permits only a missing credential file, not invalid content or unsafe access.
func TestOptionalToken(t *testing.T) {
	for _, test := range []struct {
		name    string
		data    string
		readErr error
		want    string
		fails   bool
	}{
		{"missing", "", &os.PathError{Op: "lstat", Path: "fixture", Err: os.ErrNotExist}, "", false},
		{"permission", "", os.ErrPermission, "", true},
		{"unsafe", "", errors.New("unsafe owner"), "", true},
		{"empty", "", nil, "", true},
		{"injection", "fixture\r\ninjected", nil, "", true},
		{"valid", "fixture\n", nil, "fixture", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			token, err := optionalToken([]byte(test.data), test.readErr)
			if (err != nil) != test.fails || token != test.want {
				t.Fatal("unexpected optional credential result")
			}
		})
	}
}

// TestPublicReleaseWithoutToken verifies both metadata and manifest requests omit Authorization.
func TestPublicReleaseWithoutToken(t *testing.T) {
	calls := 0
	github := &GitHub{Token: func() (string, error) { return "", nil }, Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if _, present := request.Header["Authorization"]; present {
			t.Error("anonymous request sent authorization")
		}
		var payload any = map[string]any{"tag_name": "v1.2.0", "assets": []map[string]any{{"id": 7, "name": "relay-release.json", "size": 200, "url": "https://api.github.com/repos/" + Repository + "/releases/assets/7"}}}
		if strings.Contains(request.URL.Path, "/assets/") {
			payload = testRelease()
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})}}
	release, err := github.Latest(context.Background())
	if err != nil || release != testRelease() || calls != 2 {
		t.Fatalf("public release lookup failed: %v calls=%d", err, calls)
	}
}
