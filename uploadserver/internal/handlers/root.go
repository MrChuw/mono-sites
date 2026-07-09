package handlers

import (
	"net/http"
	"uploadserver/internal/umami"
)

func RootHandler(w http.ResponseWriter, r *http.Request) {
	umami.Instance.TrackPageViewAsync(r, "Todo Root", "/")
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte("<html><body><h1>TODO</h1></body></html>"))
}
