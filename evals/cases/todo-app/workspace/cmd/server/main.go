package main

import (
	"log"
	app "miniapp"
	"net/http"
)

func main() {
	handler, err := app.NewHandler("tasks.json")
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", handler))
}
