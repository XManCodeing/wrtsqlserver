package onlineupdate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestRejectedReleaseMetadata covers private-release errors, mismatched tags and unsafe asset URLs.
func TestRejectedReleaseMetadata(t *testing.T) {
	for _, test := range []struct {
		name, tag, assetURL string
		draft, prerelease   bool
		size                int
		status              int
	}{
		{"draft", "v1.2.0", "", true, false, 200, 200},
		{"prerelease", "v1.2.0", "", false, true, 200, 200},
		{"tag mismatch", "v1.3.0", "", false, false, 200, 200},
		{"tag injection", "v1.2.0;id", "", false, false, 200, 200},
		{"asset host", "v1.2.0", "https://evil.example/asset", false, false, 200, 200},
		{"asset size", "v1.2.0", "", false, false, 16385, 200},
		{"private denied", "v1.2.0", "", false, false, 200, 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			assetURL := test.assetURL
			if assetURL == "" {
				assetURL = "https://api.github.com/repos/" + Repository + "/releases/assets/7"
			}
			metadata := map[string]any{"tag_name": test.tag, "draft": test.draft, "prerelease": test.prerelease, "assets": []map[string]any{{"id": 7, "name": "relay-release.json", "size": test.size, "url": assetURL}}}
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, _ := json.Marshal(metadata)
				if strings.Contains(request.URL.Path, "/assets/") {
					body, _ = json.Marshal(testRelease())
				}
				return &http.Response{StatusCode: test.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
			})}
			github := &GitHub{Client: client, Token: func() (string, error) { return "fixture", nil }}
			if _, err := github.Latest(context.Background()); err == nil {
				t.Fatal("unsafe release accepted")
			}
		})
	}
}

// TestFinalizeOnlyAfterExplicitApply preserves the previous rollback transaction during checks.
func TestFinalizeOnlyAfterExplicitApply(t *testing.T) {
	release := testRelease()
	store := &memoryStore{exists: true, record: Record{Status: Status{State: "succeeded", CurrentVersion: "1.0.0", CanRollback: true}}}
	host := &fakeHost{current: "1.0.0", transaction: true, applied: true}
	service := newTestService(t, host, &fakeSource{release: release}, store)
	if err := service.Check(); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if len(host.calls) != 0 {
		t.Fatal("check archived rollback transaction")
	}
	if err := service.Apply(release.Version, release.Digest, "更新 "+release.Version); err != nil {
		t.Fatal(err)
	}
	service.Wait()
	if strings.Join(host.calls, ",") != "pull,finalize,check,apply" {
		t.Fatalf("unexpected transaction order %v", host.calls)
	}
}
