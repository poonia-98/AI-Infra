package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	DatabaseURL string
	NatsURL     string
	ControlPlaneAPIKey string
	Port        string
}

func loadConfig() Config {
	return Config{
		DatabaseURL: getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:     getEnv("NATS_URL", "nats://nats:4222"),
		ControlPlaneAPIKey: getEnv("CONTROL_PLANE_API_KEY", ""),
		Port:        getEnv("PORT", "8095"),
	}
}

var (
	templateInstalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_marketplace_installs_total",
	}, []string{"template_slug"})
)

func init() {
	prometheus.MustRegister(templateInstalls)
}

type Marketplace struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	logger *slog.Logger
}

func NewMarketplace(cfg Config, db *sql.DB, nc *natsgo.Conn) *Marketplace {
	return &Marketplace{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

func (m *Marketplace) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	requireServiceAuth := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			got := c.GetHeader("X-Service-API-Key")
			if m.cfg.ControlPlaneAPIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(m.cfg.ControlPlaneAPIKey)) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "marketplace"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := m.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	protected := r.Group("/")
	protected.Use(requireServiceAuth())

	// ── Templates ─────────────────────────────────────────────────────

	// Search / list templates
	r.GET("/templates", func(c *gin.Context) {
		q := c.Query("q")
		category := c.Query("category")
		tag := c.Query("tag")
		verified := c.Query("verified") == "true"
		limit := 50

		conditions := []string{"is_public=TRUE"}
		args := []interface{}{}
		idx := 1

		if q != "" {
			conditions = append(conditions, `(name ILIKE $`+itoa(idx)+` OR description ILIKE $`+itoa(idx)+`)`)
			args = append(args, "%"+q+"%")
			idx++
		}
		if category != "" {
			conditions = append(conditions, `category=$`+itoa(idx))
			args = append(args, category)
			idx++
		}
		if tag != "" {
			conditions = append(conditions, `$`+itoa(idx)+` = ANY(tags)`)
			args = append(args, tag)
			idx++
		}
		if verified {
			conditions = append(conditions, `is_verified=TRUE`)
		}

		where := strings.Join(conditions, " AND ")
		args = append(args, limit)

		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT id::text, name, slug, description, category, tags,
			       agent_type, image, is_verified, version,
			       downloads, rating, rating_count, created_at
			FROM agent_templates
			WHERE `+where+`
			ORDER BY downloads DESC, rating DESC
			LIMIT $`+itoa(idx), args...)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		var templates []map[string]interface{}
		for rows.Next() {
			var id, name, slug, desc, cat, agentType, image, ver string
			var isVerified bool
			var downloads, ratingCount int
			var rating float64
			var tags []byte
			var createdAt time.Time
			rows.Scan(&id, &name, &slug, &desc, &cat, &tags, &agentType, &image,
				&isVerified, &ver, &downloads, &rating, &ratingCount, &createdAt)
			var parsedTags []string
			json.Unmarshal(tags, &parsedTags)
			templates = append(templates, map[string]interface{}{
				"id": id, "name": name, "slug": slug, "description": desc,
				"category": cat, "tags": parsedTags, "agent_type": agentType,
				"image": image, "is_verified": isVerified, "version": ver,
				"downloads": downloads, "rating": rating, "rating_count": ratingCount,
				"created_at": createdAt,
			})
		}
		if templates == nil {
			templates = []map[string]interface{}{}
		}
		c.JSON(200, templates)
	})

	// Get template by slug
	r.GET("/templates/:slug", func(c *gin.Context) {
		row := m.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text, name, slug, description, category, tags,
			       agent_type, image, config_schema, default_config,
			       default_env, cpu_limit, memory_limit, is_verified,
			       version, downloads, rating, rating_count, readme, created_at
			FROM agent_templates WHERE slug=$1`, c.Param("slug"))

		var id, name, slug, desc, cat, agentType, image, ver, readme string
		var isVerified bool
		var cpuLimit float64
		var downloads, ratingCount int
		var rating float64
		var tags, configSchema, defaultConfig, defaultEnv []byte
		var createdAt time.Time
		var memLimit string
		if err := row.Scan(&id, &name, &slug, &desc, &cat, &tags, &agentType,
			&image, &configSchema, &defaultConfig, &defaultEnv,
			&cpuLimit, &memLimit, &isVerified, &ver, &downloads, &rating,
			&ratingCount, &readme, &createdAt); err != nil {
			c.JSON(404, gin.H{"error": "template not found"})
			return
		}
		var parsedTags []string
		json.Unmarshal(tags, &parsedTags)
		var cs, dc, de interface{}
		json.Unmarshal(configSchema, &cs)
		json.Unmarshal(defaultConfig, &dc)
		json.Unmarshal(defaultEnv, &de)
		c.JSON(200, map[string]interface{}{
			"id": id, "name": name, "slug": slug, "description": desc,
			"category": cat, "tags": parsedTags, "agent_type": agentType,
			"image": image, "config_schema": cs, "default_config": dc,
			"default_env": de, "cpu_limit": cpuLimit, "memory_limit": memLimit,
			"is_verified": isVerified, "version": ver, "downloads": downloads,
			"rating": rating, "rating_count": ratingCount,
			"readme": readme, "created_at": createdAt,
		})
	})

	// Publish a template
	protected.POST("/templates", func(c *gin.Context) {
		var body struct {
			Name          string                 `json:"name" binding:"required"`
			Slug          string                 `json:"slug" binding:"required"`
			Description   string                 `json:"description"`
			Category      string                 `json:"category"`
			Tags          []string               `json:"tags"`
			AgentType     string                 `json:"agent_type"`
			Image         string                 `json:"image" binding:"required"`
			ConfigSchema  map[string]interface{} `json:"config_schema"`
			DefaultConfig map[string]interface{} `json:"default_config"`
			DefaultEnv    map[string]interface{} `json:"default_env"`
			CPULimit      float64                `json:"cpu_limit"`
			MemoryLimit   string                 `json:"memory_limit"`
			IsPublic      bool                   `json:"is_public"`
			Version       string                 `json:"version"`
			Readme        string                 `json:"readme"`
			CreatedBy     string                 `json:"created_by"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.AgentType == "" {
			body.AgentType = "langgraph"
		}
		if body.Version == "" {
			body.Version = "1.0.0"
		}
		if body.CPULimit == 0 {
			body.CPULimit = 1.0
		}
		if body.MemoryLimit == "" {
			body.MemoryLimit = "512m"
		}

		tagsJSON, _ := json.Marshal(body.Tags)
		csJSON, _ := json.Marshal(body.ConfigSchema)
		dcJSON, _ := json.Marshal(body.DefaultConfig)
		deJSON, _ := json.Marshal(body.DefaultEnv)

		var id string
		err := m.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO agent_templates
			  (name, slug, description, category, tags, agent_type, image,
			   config_schema, default_config, default_env, cpu_limit, memory_limit,
			   is_public, version, readme, created_by)
			VALUES ($1,$2,$3,$4,$5::text[],$6,$7,$8::jsonb,$9::jsonb,$10::jsonb,$11,$12,$13,$14,$15,$16)
			RETURNING id::text`,
			body.Name, body.Slug, body.Description, body.Category, string(tagsJSON),
			body.AgentType, body.Image, string(csJSON), string(dcJSON), string(deJSON),
			body.CPULimit, body.MemoryLimit, body.IsPublic, body.Version,
			body.Readme, body.CreatedBy).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		// Also add to registry as first version
		m.db.ExecContext(c.Request.Context(), `
			INSERT INTO agent_registry (template_id, version, image, is_latest)
			VALUES ($1::uuid, $2, $3, TRUE)`, id, body.Version, body.Image)

		c.JSON(201, gin.H{"id": id, "slug": body.Slug})
	})

	// Install template (instantiate as agent in org)
	protected.POST("/templates/:slug/install", func(c *gin.Context) {
		var body struct {
			OrganisationID string                 `json:"organisation_id" binding:"required"`
			Name           string                 `json:"name"`
			Config         map[string]interface{} `json:"config"`
			EnvVars        map[string]interface{} `json:"env_vars"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		// Load template
		var templateID, image, agentType string
		var cpuLimit float64
		var memLimit string
		var defaultConfig, defaultEnv []byte
		err := m.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text, image, agent_type, cpu_limit, memory_limit,
			       default_config, default_env
			FROM agent_templates WHERE slug=$1`, c.Param("slug")).
			Scan(&templateID, &image, &agentType, &cpuLimit, &memLimit, &defaultConfig, &defaultEnv)
		if err != nil {
			c.JSON(404, gin.H{"error": "template not found"})
			return
		}

		// Merge config
		var mergedConfig, mergedEnv map[string]interface{}
		json.Unmarshal(defaultConfig, &mergedConfig)
		json.Unmarshal(defaultEnv, &mergedEnv)
		for k, v := range body.Config {
			mergedConfig[k] = v
		}
		for k, v := range body.EnvVars {
			mergedEnv[k] = v
		}

		name := body.Name
		if name == "" {
			name = c.Param("slug") + "-" + time.Now().Format("20060102-150405")
		}

		// Increment downloads
		m.db.ExecContext(c.Request.Context(), `
			UPDATE agent_templates SET downloads=downloads+1 WHERE id=$1::uuid`, templateID)

		templateInstalls.With(prometheus.Labels{"template_slug": c.Param("slug")}).Inc()
		c.JSON(200, gin.H{
			"template_id":     templateID,
			"name":            name,
			"image":           image,
			"agent_type":      agentType,
			"cpu_limit":       cpuLimit,
			"memory_limit":    memLimit,
			"config":          mergedConfig,
			"env_vars":        mergedEnv,
			"organisation_id": body.OrganisationID,
		})
	})

	// Submit review
	protected.POST("/templates/:slug/reviews", func(c *gin.Context) {
		var body struct {
			UserID string `json:"user_id"`
			Rating int    `json:"rating" binding:"required,min=1,max=5"`
			Review string `json:"review"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var templateID string
		m.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text FROM agent_templates WHERE slug=$1`, c.Param("slug")).Scan(&templateID)
		if templateID == "" {
			c.JSON(404, gin.H{"error": "template not found"})
			return
		}
		m.db.ExecContext(c.Request.Context(), `
			INSERT INTO template_reviews (template_id, user_id, rating, review)
			VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4)`,
			templateID, body.UserID, body.Rating, body.Review)
		// Update average rating
		m.db.ExecContext(c.Request.Context(), `
			UPDATE agent_templates SET
			  rating=(SELECT AVG(rating) FROM template_reviews WHERE template_id=$1::uuid),
			  rating_count=(SELECT COUNT(*) FROM template_reviews WHERE template_id=$1::uuid)
			WHERE id=$1::uuid`, templateID)
		c.JSON(201, gin.H{"status": "ok"})
	})

	// List registry versions for a template
	r.GET("/templates/:slug/versions", func(c *gin.Context) {
		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT ar.id::text, ar.version, ar.image, ar.image_digest,
			       ar.is_latest, ar.published_at
			FROM agent_registry ar
			JOIN agent_templates at ON at.id=ar.template_id
			WHERE at.slug=$1
			ORDER BY ar.published_at DESC`, c.Param("slug"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var versions []map[string]interface{}
		for rows.Next() {
			var id, version, image, digest string
			var isLatest bool
			var publishedAt time.Time
			rows.Scan(&id, &version, &image, &digest, &isLatest, &publishedAt)
			versions = append(versions, map[string]interface{}{
				"id": id, "version": version, "image": image,
				"digest": digest, "is_latest": isLatest, "published_at": publishedAt,
			})
		}
		if versions == nil {
			versions = []map[string]interface{}{}
		}
		c.JSON(200, versions)
	})

	// Get categories
	r.GET("/categories", func(c *gin.Context) {
		rows, err := m.db.QueryContext(c.Request.Context(), `
			SELECT category, COUNT(*) as count
			FROM agent_templates WHERE is_public=TRUE AND category IS NOT NULL
			GROUP BY category ORDER BY count DESC`)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var cats []map[string]interface{}
		for rows.Next() {
			var cat string
			var count int
			rows.Scan(&cat, &count)
			cats = append(cats, map[string]interface{}{"category": cat, "count": count})
		}
		if cats == nil {
			cats = []map[string]interface{}{}
		}
		c.JSON(200, cats)
	})

	return r
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting marketplace-service", "port", cfg.Port)

	var db *sql.DB
	var err error
	for i := 0; i < 30; i++ {
		db, err = sql.Open("postgres", cfg.DatabaseURL)
		if err == nil && db.PingContext(context.Background()) == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		logger.Error("postgres unavailable", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(15)
	defer db.Close()

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1),
			natsgo.Name("marketplace"))
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		logger.Error("nats unavailable", "error", err)
		os.Exit(1)
	}
	defer nc.Close()

	mp := NewMarketplace(cfg, db, nc)
	router := mp.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	result := make([]byte, 0, 3)
	for n > 0 {
		result = append([]byte{byte('0' + n%10)}, result...)
		n /= 10
	}
	return string(result)
}
