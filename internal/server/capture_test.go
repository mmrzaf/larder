package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mmrzaf/larder/testutil"
)

func TestCaptureTokenAndPin(t *testing.T) {
	ts := newTestServer(t, genericImmutableRepo(), nil)
	ts.upstream.Set("/file", testutil.Route{Body: []byte("captured content")})
	startBody, _ := json.Marshal(map[string]any{"set": "captured", "from": []string{}})
	response, err := http.Post(ts.adminURL("/_larder/v1/capture/start"), "application/json", bytes.NewReader(startBody))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if started.Token == "" {
		t.Fatal("capture token is empty")
	}
	request, _ := http.NewRequest(http.MethodGet, ts.publicURL("/generic/file"), nil)
	request.Header.Set("X-Larder-Capture", started.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fetch status = %d", response.StatusCode)
	}
	stopBody, _ := json.Marshal(map[string]any{"token": started.Token, "pin": true})
	response, err = http.Post(ts.adminURL("/_larder/v1/capture/stop"), "application/json", bytes.NewReader(stopBody))
	if err != nil {
		t.Fatal(err)
	}
	var stopped struct {
		Set    string `json:"set"`
		Keys   int    `json:"keys"`
		Pinned int    `json:"pinned"`
	}
	if err := json.NewDecoder(response.Body).Decode(&stopped); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if stopped.Set != "captured" || stopped.Keys != 1 || stopped.Pinned != 1 {
		t.Fatalf("stopped = %+v", stopped)
	}
	response, err = http.Get(ts.adminURL("/_larder/v1/pins"))
	if err != nil {
		t.Fatal(err)
	}
	var pins struct {
		Pins []struct {
			Name           string `json:"name"`
			MatchedObjects int64  `json:"matched_objects"`
		} `json:"pins"`
	}
	if err := json.NewDecoder(response.Body).Decode(&pins); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	found := false
	for _, pin := range pins.Pins {
		if pin.Name == "captured" && pin.MatchedObjects == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("capture did not pin the fetched key: %+v", pins.Pins)
	}
}

func TestCaptureCIDRAndExpiry(t *testing.T) {
	ts := newTestServer(t, genericImmutableRepo(), nil)
	ts.upstream.Set("/file", testutil.Route{Body: []byte("cidr content")})
	startBody, _ := json.Marshal(map[string]any{"set": "cidr", "from": []string{"127.0.0.1/32"}})
	response, err := http.Post(ts.adminURL("/_larder/v1/capture/start"), "application/json", bytes.NewReader(startBody))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(response.Body).Decode(&started)
	_ = response.Body.Close()
	response, err = http.Get(ts.publicURL("/generic/file"))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	stopBody, _ := json.Marshal(map[string]any{"token": started.Token, "pin": false})
	response, err = http.Post(ts.adminURL("/_larder/v1/capture/stop"), "application/json", bytes.NewReader(stopBody))
	if err != nil {
		t.Fatal(err)
	}
	var stopped struct {
		Keys int `json:"keys"`
	}
	_ = json.NewDecoder(response.Body).Decode(&stopped)
	_ = response.Body.Close()
	if stopped.Keys != 1 {
		t.Fatalf("CIDR capture keys = %d", stopped.Keys)
	}

	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	captures := newCaptureStore(clock)
	token, err := captures.start("expiring", nil)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if _, _, _, err := captures.stop(token, false, nil); err == nil {
		t.Fatal("expired capture was accepted")
	}
}
