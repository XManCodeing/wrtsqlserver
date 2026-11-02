package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// failingSecretReader simulates a secret source failing after returning partial bytes.
type failingSecretReader struct{}

// Read returns partial data and an injected error to ensure partial secrets are never accepted.
func (failingSecretReader) Read(buffer []byte) (int, error) {
	return copy(buffer, "partial-secret"), errors.New("injected read error")
}

// TestPasswordReaderShortReads verifies complete reads, newline handling and bounded failures.
func TestPasswordReaderShortReads(t *testing.T) {
	secret := strings.Repeat("x", 4096)
	cases := []struct {
		name   string
		reader io.Reader
		want   string
		failed bool
	}{
		{"one byte reads", iotest.OneByteReader(strings.NewReader("long-secret-value\r\n")), "long-secret-value", false},
		{"max length CRLF", iotest.HalfReader(strings.NewReader(secret + "\r\n")), secret, false},
		{"data and EOF", iotest.DataErrReader(strings.NewReader("long-secret-value")), "long-secret-value", false},
		{"oversize", strings.NewReader(strings.Repeat("x", 5000)), "", true},
		{"partial error", failingSecretReader{}, "", true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			actual, err := readPasswordFile(testCase.reader)
			if (err != nil) != testCase.failed || actual != testCase.want {
				t.Fatalf("unexpected read result: error=%v length=%d", err, len(actual))
			}
		})
	}
}

// TestStoredCredentialPairs rejects incomplete persisted authentication before starting listeners.
func TestStoredCredentialPairs(t *testing.T) {
	for _, credentials := range []struct {
		username, password string
		valid              bool
	}{
		{"", "", true}, {"user", "secret", true}, {"user", "", false}, {"", "secret", false},
	} {
		store, err := newStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		proxy := Proxy{ID: "proxy", Name: "Proxy", Host: "127.0.0.1", Port: 1080, Username: credentials.username, Password: credentials.password}
		if err := store.Save(Config{Proxies: []Proxy{proxy}, Mappings: []Mapping{}}); err != nil {
			t.Fatal(err)
		}
		app, err := newApp(store, "test-password-long", "127.0.0.1:8080", "", 1)
		if app != nil {
			app.Close()
		}
		if (err == nil) != credentials.valid {
			t.Fatalf("unexpected persisted authentication result: %v", err)
		}
	}
}

// TestAPICredentialPairs verifies incomplete pairs fail while the no-auth configuration remains supported.
func TestAPICredentialPairs(t *testing.T) {
	app := testApp(t)
	cookie, csrf := loginTest(t, app)
	for _, body := range []string{
		`{"name":"Proxy","host":"localhost","port":1080,"username":"user"}`,
		`{"name":"Proxy","host":"localhost","port":1080,"password":"secret"}`,
	} {
		response := requestAPI(app, "POST", "/api/proxies", body, cookie, csrf)
		if response.Code != 400 {
			t.Fatal("incomplete authentication accepted", response.Code)
		}
	}
	if len(app.cfg.Proxies) != 0 {
		t.Fatal("invalid authentication persisted")
	}
	response := requestAPI(app, "POST", "/api/proxies", `{"name":"Proxy","host":"localhost","port":1080}`, cookie, csrf)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
}
