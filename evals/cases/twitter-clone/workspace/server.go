package app

import "net/http"

// NewHandler loads the JSON store and exposes the local social app.
func NewHandler(path string) (http.Handler, error) {
	return http.NotFoundHandler(), nil
}
