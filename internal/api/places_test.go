package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/api"
	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
	"git.marcorealacci.me/marcorealacci/tramite/internal/places"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type stubProvider struct{ fail error }

func (p stubProvider) Autocomplete(_ context.Context, q places.Query) ([]places.Suggestion, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	return []places.Suggestion{{ID: "P1", Name: "Colosseo", Kind: "place"}}, nil
}

func (p stubProvider) Locate(_ context.Context, id, _, _ string) (places.Location, error) {
	if id != "P1" {
		return places.Location{}, places.ErrNotFound
	}
	return places.Location{Lat: 41.89, Lon: 12.49}, nil
}

func placesServer(t *testing.T, p places.Provider, limit config.MonthlyLimit, perMinute int) *httptest.Server {
	t.Helper()
	_, _, svc := setup(t)
	b := places.NewBudget(filepath.Join(t.TempDir(), "u.json"), limit, time.Now, quiet)
	s := places.NewService("google", "Google Maps", p, b, places.NewLimiter(perMinute, 0, time.Now), quiet)
	srv := httptest.NewServer((&api.API{Svc: svc, Places: s, ClientIPHeader: "X-Forwarded-For"}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, header ...string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return resp, body
}

func TestPlacesEndpoints(t *testing.T) {
	srv := placesServer(t, stubProvider{}, config.MonthlyLimit{Autocomplete: 100, Details: 100}, 0)
	resp, body := getJSON(t, srv.URL+"/v1/places/autocomplete?q=colos&near=41.9,12.5&session=S")
	res := body["results"].([]any)
	if resp.StatusCode != 200 || body["attribution"] != "Google Maps" || res[0].(map[string]any)["id"] != "google:P1" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	resp, body = getJSON(t, srv.URL+"/v1/places/google:P1?session=S")
	if resp.StatusCode != 200 || body["lat"] != 41.89 || body["id"] != "google:P1" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	for path, want := range map[string]int{
		"/v1/places/autocomplete":                  400,
		"/v1/places/autocomplete?q=a&near=1":       400,
		"/v1/places/autocomplete?q=a&near=100,200": 400,
		"/v1/places/google:NOPE":                   404,
		"/v1/places/other:P1":                      404,
	} {
		if resp, _ := getJSON(t, srv.URL+path); resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	bad := placesServer(t, stubProvider{fail: errors.New("boom")}, config.MonthlyLimit{Autocomplete: 100, Details: 100}, 0)
	if resp, body := getJSON(t, bad.URL+"/v1/places/autocomplete?q=a"); resp.StatusCode != 502 || body["error"] != "upstream" {
		t.Fatalf("upstream error: %d %v", resp.StatusCode, body)
	}
}

func TestPlacesLimits(t *testing.T) {
	srv := placesServer(t, stubProvider{}, config.MonthlyLimit{Autocomplete: 100, Details: 100}, 2)
	q := srv.URL + "/v1/places/autocomplete?q=a"
	getJSON(t, q, "X-Forwarded-For", "6.6.6.6, 1.1.1.1")
	getJSON(t, q, "X-Forwarded-For", "7.7.7.7, 1.1.1.1") // same last hop: same client
	resp, body := getJSON(t, q, "X-Forwarded-For", "1.1.1.1")
	if resp.StatusCode != 429 || body["error"] != "rate_limit" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %v %q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if resp, _ := getJSON(t, q, "X-Forwarded-For", "2.2.2.2"); resp.StatusCode != 200 {
		t.Fatalf("other client: %d", resp.StatusCode)
	}

	quota := placesServer(t, stubProvider{}, config.MonthlyLimit{Autocomplete: 1, Details: 100}, 0)
	getJSON(t, quota.URL+"/v1/places/autocomplete?q=a")
	resp, body = getJSON(t, quota.URL+"/v1/places/autocomplete?q=a")
	if resp.StatusCode != 503 || body["error"] != "quota" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("quota: %d %v", resp.StatusCode, body)
	}
}
