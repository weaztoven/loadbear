package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := flag.Int("port", 8081, "port to bind")
	name := flag.String("name", "backend", "backend name")
	mode := flag.String("mode", "normal", "response mode: normal, hang, close, drop-mid, or slow")
	flag.Parse()
	if !validMode(*mode) {
		log.Fatalf("invalid mode %q; valid modes are normal, hang, close, drop-mid, slow", *mode)
	}
	mux := newHandler(*name, *mode)
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("fakebackend %s listening on %s (mode %s)", *name, addr, *mode)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
}

func newHandler(name, defaultMode string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = defaultMode
		}
		if !validMode(mode) {
			http.Error(w, "invalid mode\n", http.StatusBadRequest)
			return
		}
		switch mode {
		case "hang":
			<-r.Context().Done()
			return
		case "close":
			hijackAndClose(w)
			return
		case "drop-mid":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"backend":"`+name+`","partial":`)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			hijackAndClose(w)
			return
		case "slow":
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"backend":"%s","path":"%s"}`+"\n", name, r.URL.Path)
	})
	return mux
}

func validMode(mode string) bool {
	switch mode {
	case "normal", "hang", "close", "drop-mid", "slow":
		return true
	default:
		return false
	}
}

func hijackAndClose(w http.ResponseWriter) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection hijacking unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}
