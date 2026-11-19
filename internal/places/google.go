package places

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"git.marcorealacci.me/marcorealacci/tramite/internal/config"
)

// Google is the Places API (New): Autocomplete with session tokens, then
// Place Details asking for the location only (the "Essentials" tier, whose
// call also makes the session's suggestions free).
type Google struct {
	Key    string
	Region string
	// Bias when the client sends no position.
	Bias *config.Circle
	// RadiusMeters around the client's position that results are favoured in.
	RadiusMeters float64
	// BaseURL: https://places.googleapis.com unless set (tests).
	BaseURL string
	Client  *http.Client
}

const googleBase = "https://places.googleapis.com"

func (g *Google) base() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	return googleBase
}

func (g *Google) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 8 * time.Second}
}

type gLatLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type gText struct {
	Text string `json:"text"`
}

func (g *Google) Autocomplete(ctx context.Context, q Query) ([]Suggestion, error) {
	body := map[string]any{"input": q.Text}
	if q.Lang != "" {
		body["languageCode"] = q.Lang
	}
	if g.Region != "" {
		body["regionCode"] = g.Region
	}
	if q.Session != "" {
		body["sessionToken"] = q.Session
	}
	if q.Near != nil {
		c := gLatLng{q.Near.Lat, q.Near.Lon}
		body["origin"] = c
		body["locationBias"] = map[string]any{"circle": map[string]any{"center": c, "radius": g.RadiusMeters}}
	} else if b := g.Bias; b != nil {
		body["locationBias"] = map[string]any{"circle": map[string]any{
			"center": gLatLng{b.Lat, b.Lon}, "radius": b.RadiusMeters}}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", g.base()+"/v1/places:autocomplete", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	var res struct {
		Suggestions []struct {
			PlacePrediction *struct {
				PlaceID          string `json:"placeId"`
				Text             gText  `json:"text"`
				StructuredFormat struct {
					MainText      gText `json:"mainText"`
					SecondaryText gText `json:"secondaryText"`
				} `json:"structuredFormat"`
				Types          []string `json:"types"`
				DistanceMeters *int     `json:"distanceMeters"`
			} `json:"placePrediction"`
		} `json:"suggestions"`
	}
	if err := g.do(req, &res); err != nil {
		return nil, err
	}
	out := []Suggestion{}
	for _, s := range res.Suggestions {
		p := s.PlacePrediction
		if p == nil || p.PlaceID == "" {
			continue // a query prediction ("pizza near…"): not a place
		}
		name := p.StructuredFormat.MainText.Text
		if name == "" {
			name = p.Text.Text
		}
		kind := "place"
		if isAddress(p.Types) {
			kind = "address"
		}
		out = append(out, Suggestion{ID: p.PlaceID, Name: name, Subtitle: p.StructuredFormat.SecondaryText.Text,
			Kind: kind, DistanceMeters: p.DistanceMeters})
	}
	return out, nil
}

// isAddress: a street, a house number, a building; not a named place.
func isAddress(types []string) bool {
	if slices.Contains(types, "establishment") || slices.Contains(types, "point_of_interest") {
		return false
	}
	for _, t := range []string{"street_address", "premise", "subpremise", "route", "street_number", "intersection", "plus_code"} {
		if slices.Contains(types, t) {
			return true
		}
	}
	return false
}

func (g *Google) Locate(ctx context.Context, id, session, lang string) (Location, error) {
	v := url.Values{}
	if session != "" {
		v.Set("sessionToken", session)
	}
	if lang != "" {
		v.Set("languageCode", lang)
	}
	u := g.base() + "/v1/places/" + url.PathEscape(id)
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return Location{}, err
	}
	// Location only: the Essentials tier (displayName would be Pro).
	req.Header.Set("X-Goog-FieldMask", "location")
	var res struct {
		Location *gLatLng `json:"location"`
	}
	if err := g.do(req, &res); err != nil {
		return Location{}, err
	}
	if res.Location == nil {
		return Location{}, ErrNotFound
	}
	return Location{Lat: res.Location.Latitude, Lon: res.Location.Longitude}, nil
}

func (g *Google) do(req *http.Request, into any) error {
	req.Header.Set("X-Goog-Api-Key", g.Key)
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return json.Unmarshal(raw, into)
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: google %d: %s", ErrUpstreamLimit, resp.StatusCode, short(raw))
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusBadRequest && req.Method == "GET":
		// A bad or stale place id.
		return fmt.Errorf("%w: google %d: %s", ErrNotFound, resp.StatusCode, short(raw))
	default:
		return fmt.Errorf("google %d: %s", resp.StatusCode, short(raw))
	}
}

func short(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}
