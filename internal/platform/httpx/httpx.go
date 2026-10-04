package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/hmza-hb/crawler/internal/platform/config"
	"github.com/hmza-hb/crawler/internal/platform/observe"
)

type APIError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *APIError) WithCause(err error) *APIError {
	e.Cause = err
	return e
}

func BadRequest(msg string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Message: msg}
}

func NotFound(msg string) *APIError {
	return &APIError{Status: http.StatusNotFound, Message: msg}
}

func Internal(msg string) *APIError {
	return &APIError{Status: http.StatusInternalServerError, Message: msg}
}

func Timeout(msg string) *APIError {
	return &APIError{Status: http.StatusGatewayTimeout, Message: msg}
}

func Error(w http.ResponseWriter, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		JSON(w, apiErr.Status, map[string]any{"error": apiErr.Message})
		return
	}
	JSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func DecodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dest any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dest)
}

func Handler(fn any) http.Handler {
	switch h := fn.(type) {
	case http.HandlerFunc:
		return h
	case func(w http.ResponseWriter, r *http.Request):
		return http.HandlerFunc(h)
	case func(w http.ResponseWriter, r *http.Request) error:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := h(w, r); err != nil {
				Error(w, err)
			}
		})
	default:
		panic(fmt.Sprintf("httpx.Handler: unsupported handler type %T", fn))
	}
}

func MethodNotAllowedHandler(allowed string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		JSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	})
}

type ServerOption func(*Server)

type Server struct {
	cfg       config.HTTPConfig
	log       *slog.Logger
	version   string
	readiness func(context.Context) error
}

func NewServer(cfg config.HTTPConfig, log *slog.Logger, opts ...ServerOption) *Server {
	s := &Server{
		cfg: cfg,
		log: log,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func WithVersion(v string) ServerOption {
	return func(s *Server) { s.version = v }
}

func WithMetrics(r *observe.Registry) ServerOption {
	return func(s *Server) {}
}

func WithReadiness(fn func(context.Context) error) ServerOption {
	return func(s *Server) { s.readiness = fn }
}

func (s *Server) Run(ctx context.Context, handler http.Handler) error {
	srv := &http.Server{
		Addr:         s.cfg.Addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	errChan := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if r := recover(); r != nil {
					logger.Error("panic recovered", "panic", r)
					JSON(w, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func RequestID(header string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}
}
