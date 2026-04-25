package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal)
	prometheus.MustRegister(httpRequestDuration)
}

// ---------------------------------------------------------------------------
// Prometheus middleware
// ---------------------------------------------------------------------------

func prometheusMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		duration := time.Since(start).Seconds()
		status := fmt.Sprintf("%d", c.Writer.Status())
		httpRequestsTotal.WithLabelValues(c.Request.Method, c.FullPath(), status).Inc()
		httpRequestDuration.WithLabelValues(c.Request.Method, c.FullPath()).Observe(duration)
	}
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

var db *sql.DB

func initDB() {
	host := getEnv("DB_HOST", "localhost")
	port := getEnv("DB_PORT", "5432")
	user := getEnv("DB_USER", "postgres")
	password := getEnv("DB_PASSWORD", "password")
	dbname := getEnv("DB_NAME", "appdb")

	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname,
	)

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Printf("Warning: could not open DB connection: %v", err)
		return
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func healthLive(c *gin.Context) {
	jsonResponse(c, http.StatusOK, gin.H{"status": "alive"})
}

func healthReady(c *gin.Context) {
	if db != nil {
		if err := db.Ping(); err != nil {
			jsonResponse(c, http.StatusServiceUnavailable, gin.H{
				"status": "not ready",
				"error":  "database unreachable",
			})
			return
		}
	}
	jsonResponse(c, http.StatusOK, gin.H{"status": "ready"})
}

func welcome(c *gin.Context) {
	// TODO: เปลี่ยนข้อความนี้เพื่อทดสอบ CI/CD
	jsonResponse(c, http.StatusOK, gin.H{
		"message": "Hello from Go API v2.0.1 ✨",
		"time":    time.Now().UTC(),
	})
}

func getItems(c *gin.Context) {
	if db == nil {
		jsonResponse(c, http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}

	rows, err := db.QueryContext(c.Request.Context(), "SELECT id, name FROM items ORDER BY id")
	if err != nil {
		jsonResponse(c, http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	type Item struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Name); err != nil {
			jsonResponse(c, http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		items = append(items, it)
	}
	jsonResponse(c, http.StatusOK, items)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	initDB()

	gin.SetMode(getEnv("GIN_MODE", "release"))
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), prometheusMiddleware())

	// Health probes (not counted in business metrics – registered before middleware)
	r.GET("/livez", healthLive)
	r.GET("/readyz", healthReady)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Business routes
	r.GET("/", welcome)
	r.GET("/items", getItems)

	port := getEnv("PORT", "8080")
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("Server listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// ---------------------------------------------------------------------------
	// Graceful Shutdown – catch SIGTERM / SIGINT
	// ---------------------------------------------------------------------------
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutdown signal received – draining connections...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Forced shutdown: %v", err)
	}
	log.Println("Server exited gracefully")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func jsonResponse(c *gin.Context, code int, obj any) {
	b, _ := json.Marshal(obj)
	c.Data(code, "application/json; charset=utf-8", append(b, '\n'))
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
