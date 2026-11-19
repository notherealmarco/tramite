package api

import (
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"git.marcorealacci.me/marcorealacci/tramite/internal/places"
)

const maxPlaceQuery = 200 // characters

// placesOff: this server has no place search (no provider or no key).
func (a *API) placesOff(w http.ResponseWriter) bool {
	if a.Places == nil {
		placeErr(w, http.StatusNotImplemented, "unavailable", 0)
		return true
	}
	return false
}

// placeSuggestions: GET /v1/places/autocomplete?q=&near=lat,lon&lang=&session=
func (a *API) placeSuggestions(w http.ResponseWriter, r *http.Request) {
	if a.placesOff(w) || !a.admit(w, r) {
		return
	}
	qs := r.URL.Query()
	q := places.Query{Text: strings.TrimSpace(qs.Get("q")), Lang: qs.Get("lang"), Session: qs.Get("session")}
	if q.Text == "" || utf8.RuneCountInString(q.Text) > maxPlaceQuery {
		placeErr(w, http.StatusBadRequest, "q must be 1.."+strconv.Itoa(maxPlaceQuery)+" characters", 0)
		return
	}
	if near := qs.Get("near"); near != "" {
		p, err := parseLatLon(near)
		if err != nil {
			placeErr(w, http.StatusBadRequest, err.Error(), 0)
			return
		}
		q.Near = &p
	}
	res, err := a.Places.Autocomplete(r.Context(), q)
	if err != nil {
		a.placeFailed(w, err)
		return
	}
	writeJSON(w, r, struct {
		Attribution string              `json:"attribution"`
		Results     []places.Suggestion `json:"results"`
	}{a.Places.Attribution, res})
}

// placeLocation: GET /v1/places/{id}?session=&lang=
func (a *API) placeLocation(w http.ResponseWriter, r *http.Request) {
	if a.placesOff(w) || !a.admit(w, r) {
		return
	}
	qs := r.URL.Query()
	loc, err := a.Places.Locate(r.Context(), r.PathValue("id"), qs.Get("session"), qs.Get("lang"))
	if err != nil {
		a.placeFailed(w, err)
		return
	}
	writeJSON(w, r, loc)
}

func (a *API) admit(w http.ResponseWriter, r *http.Request) bool {
	if err := a.Places.Admit(a.clientIP(r)); err != nil {
		a.placeFailed(w, err)
		return false
	}
	return true
}

func (a *API) placeFailed(w http.ResponseWriter, err error) {
	var lim *places.LimitError
	switch {
	case errors.As(err, &lim) && lim.Reason == "rate_limit":
		placeErr(w, http.StatusTooManyRequests, lim.Reason, lim.RetryAfter.Seconds())
	case errors.As(err, &lim):
		placeErr(w, http.StatusServiceUnavailable, lim.Reason, lim.RetryAfter.Seconds())
	case errors.Is(err, places.ErrNotFound):
		placeErr(w, http.StatusNotFound, "not_found", 0)
	default:
		placeErr(w, http.StatusBadGateway, "upstream", 0)
	}
}

// placeErr answers {"error": reason}, with Retry-After (whole seconds,
// rounded up) when retryAfter > 0.
func placeErr(w http.ResponseWriter, status int, reason string, retryAfter float64) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter))))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": reason})
}

// clientIP: the connection's address, or behind a reverse proxy the one in
// the configured header (for X-Forwarded-For the last hop's, the one our
// proxy added: earlier ones are the client's to forge).
func (a *API) clientIP(r *http.Request) string {
	if h := a.ClientIPHeader; h != "" {
		if v := r.Header.Values(h); len(v) > 0 {
			parts := strings.Split(v[len(v)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parseLatLon(s string) (places.LatLon, error) {
	bad := errors.New("near must be lat,lon")
	p := strings.Split(s, ",")
	if len(p) != 2 {
		return places.LatLon{}, bad
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(p[0]), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(p[1]), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return places.LatLon{}, bad
	}
	return places.LatLon{Lat: lat, Lon: lon}, nil
}
