package metrics

import (
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	server *http.Server
	enabled bool
}

func (s *Server) Start() error {
	if !s.enabled {
		return nil
	}
	return s.server.ListenAndServe()
}

func (s *Server) Stop() error {
	if !s.enabled {
		return nil
	}
	return s.server.Close()
}

func NewServer(config Config) *Server {
	if !config.IsEnabled() {
		return &Server{
			enabled: false,
		}
	}

	r := http.NewServeMux()

	// Add health check endpoint
	r.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	r.Handle("/metrics", promhttp.Handler())

	return &Server{
		server: &http.Server{
			Handler:           r,
			Addr:              fmt.Sprintf(":%d", config.Port),
			WriteTimeout:      time.Second * 15,
			ReadTimeout:       time.Second * 15,
			ReadHeaderTimeout: time.Second * 5,
		},
		enabled: true,
	}
}
