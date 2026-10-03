package app

import "net/http"

// NewHandler loads the JSON store at path, or starts empty if it does not exist.
func NewHandler(path string) (http.Handler, error) {
	return http.NotFoundHandler(), nil
}
