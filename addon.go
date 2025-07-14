package main

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
)

// Addon represents a local addon
type Addon struct {
	Manifest map[string]interface{}
	handlers map[string]http.HandlerFunc
}

// NewAddon creates a new addon instance
func NewAddon(manifest map[string]interface{}) *Addon {
	return &Addon{
		Manifest: manifest,
		handlers: make(map[string]http.HandlerFunc),
	}
}

// GetRouter creates and returns a new router for the addon
func (a *Addon) GetRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/manifest.json", a.handleManifest).Methods("GET")
	router.HandleFunc("/{resource}/{type}/{id}/{extra}.json", a.handleResource).Methods("GET")
	router.HandleFunc("/{resource}/{type}/{id}", a.handleResource).Methods("GET")
	return router
}

func (a *Addon) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(a.Manifest)
}

func (a *Addon) handleResource(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	resource := vars["resource"]

	if handler, ok := a.handlers[resource]; ok {
		handler(w, r)
		return
	}

	http.NotFound(w, r)
}
