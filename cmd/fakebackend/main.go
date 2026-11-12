package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := flag.Int("port", 8081, "port to bind")
	name := flag.String("name", "backend", "backend name")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"backend":"%s","path":"%s"}`+"\n", *name, r.URL.Path)
	})
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("fakebackend %s listening on %s", *name, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
}
