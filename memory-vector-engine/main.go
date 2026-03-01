package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	DatabaseURL    string
	NatsURL        string
	EmbeddingURL   string // external embedding API (OpenAI-compatible)
	EmbeddingModel string
	Port           string
}

func loadConfig() Config {
	return Config{
		DatabaseURL:    getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:        getEnv("NATS_URL", "nats://nats:4222"),
		EmbeddingURL:   getEnv("EMBEDDING_API_URL", "http://embedding-service:8080"),
		EmbeddingModel: getEnv("EMBEDDING_MODEL", "text-embedding-ada-002"),
		Port:           getEnv("PORT", "8100"),
	}
}

var (
	embeddingsGenerated = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_memory_embeddings_generated_total"})
	memoriesStored      = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_memory_memories_stored_total"})
	searchRequests      = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_memory_searches_total"})
)

func init() {
	prometheus.MustRegister(embeddingsGenerated, memoriesStored, searchRequests)
}

type VectorEngine struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *VectorEngine {
	return &VectorEngine{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 30 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Embedding generation ───────────────────────────────────────────────────

func (ve *VectorEngine) GenerateEmbedding(ctx context.Context, text string) ([]float64, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"model": ve.cfg.EmbeddingModel,
		"input": text,
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		ve.cfg.EmbeddingURL+"/v1/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := ve.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding API: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &result); err != nil || len(result.Data) == 0 {
		return nil, fmt.Errorf("embedding parse failed")
	}

	embeddingsGenerated.Inc()
	return result.Data[0].Embedding, nil
}

func floatsToVector(floats []float64) string {
	if len(floats) == 0 {
		return ""
	}
	result := "["
	for i, f := range floats {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf("%g", f)
	}
	return result + "]"
}

// ── Memory operations ──────────────────────────────────────────────────────

func (ve *VectorEngine) StoreMemoryWithEmbedding(ctx context.Context,
	agentID, orgID, content, memType, key, sessionID string,
	importance float64, metadata map[string]interface{}) (string, error) {

	embedding, err := ve.GenerateEmbedding(ctx, content)
	if err != nil {
		ve.logger.Warn("embedding generation failed, storing without embedding", "error", err)
	}

	metaJSON, _ := json.Marshal(metadata)
	embeddingStr := floatsToVector(embedding)

	var id string
	err = ve.db.QueryRowContext(ctx, `
		INSERT INTO agent_memory
		  (agent_id, organisation_id, memory_type, key, content, embedding,
		   metadata, importance, session_id, embedding_model, embedding_dim)
		VALUES
		  ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5,
		   CASE WHEN $6='' THEN NULL ELSE $6::vector END,
		   $7::jsonb, $8, $9, $10, $11)
		RETURNING id::text`,
		agentID, orgID, memType, key, content,
		embeddingStr, string(metaJSON), importance, sessionID,
		ve.cfg.EmbeddingModel, len(embedding)).Scan(&id)
	if err != nil {
		return "", err
	}

	memoriesStored.Inc()
	return id, nil
}

func (ve *VectorEngine) SemanticSearch(ctx context.Context,
	agentID, query, memType string, limit int, threshold float64) ([]map[string]interface{}, error) {

	embedding, err := ve.GenerateEmbedding(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query embedding failed: %w", err)
	}

	embStr := floatsToVector(embedding)
	minDist := 1.0 - threshold

	conditions := "agent_id=$1::uuid AND embedding IS NOT NULL AND (expires_at IS NULL OR expires_at > NOW())"
	params := []interface{}{agentID, embStr, minDist, limit}
	if memType != "" {
		conditions += " AND memory_type=$5"
		params = append(params, memType)
	}

	query_sql := fmt.Sprintf(`
		SELECT id::text, memory_type, key, content, metadata, importance,
		       (1 - (embedding <=> $2::vector)) as similarity
		FROM agent_memory
		WHERE %s
		  AND (1 - (embedding <=> $2::vector)) >= (1 - $3)
		ORDER BY embedding <=> $2::vector
		LIMIT $4`, conditions)

	rows, err := ve.db.QueryContext(ctx, query_sql, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, memType2, key, content string
		var metaJSON []byte
		var importance, similarity float64
		rows.Scan(&id, &memType2, &key, &content, &metaJSON, &importance, &similarity)
		var meta interface{}
		json.Unmarshal(metaJSON, &meta)
		results = append(results, map[string]interface{}{
			"id": id, "memory_type": memType2, "key": key, "content": content,
			"metadata": meta, "importance": importance, "similarity": similarity,
		})
	}

	searchRequests.Inc()
	if results == nil {
		results = []map[string]interface{}{}
	}
	return results, nil
}

// ── Knowledge base operations ──────────────────────────────────────────────

func (ve *VectorEngine) IndexDocument(ctx context.Context, kbID, orgID, content, sourceURL, docID string) (int, error) {
	// Load KB config
	var chunkSize, chunkOverlap, embDim int
	var embModel string
	err := ve.db.QueryRowContext(ctx, `
		SELECT chunk_size, chunk_overlap, embedding_dim, embedding_model
		FROM knowledge_bases WHERE id=$1::uuid`, kbID).
		Scan(&chunkSize, &chunkOverlap, &embDim, &embModel)
	if err != nil {
		return 0, fmt.Errorf("knowledge base not found")
	}

	// Chunk document
	chunks := chunkText(content, chunkSize, chunkOverlap)
	var stored int

	for i, chunk := range chunks {
		embedding, _ := ve.GenerateEmbedding(ctx, chunk)
		embStr := floatsToVector(embedding)

		ve.db.ExecContext(ctx, `
			INSERT INTO knowledge_chunks (kb_id, organisation_id, source_url, source_doc_id, content, embedding, chunk_index, token_count)
			VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5,
			        CASE WHEN $6='' THEN NULL ELSE $6::vector END, $7, $8)`,
			kbID, orgID, sourceURL, docID, chunk, embStr, i, len(chunk)/4)
		stored++
	}

	// Update doc/chunk count in KB
	ve.db.ExecContext(ctx, `
		UPDATE knowledge_bases
		SET chunk_count=chunk_count+$1, doc_count=doc_count+1, updated_at=NOW()
		WHERE id=$2::uuid`, stored, kbID)

	return stored, nil
}

func (ve *VectorEngine) SearchKnowledge(ctx context.Context, kbID, query string, limit int, threshold float64) ([]map[string]interface{}, error) {
	embedding, err := ve.GenerateEmbedding(ctx, query)
	if err != nil {
		return nil, err
	}

	embStr := floatsToVector(embedding)
	rows, err := ve.db.QueryContext(ctx, `
		SELECT id::text, content, source_url, source_doc_id, chunk_index,
		       (1 - (embedding <=> $2::vector)) as similarity
		FROM knowledge_chunks
		WHERE kb_id=$1::uuid AND embedding IS NOT NULL
		  AND (1 - (embedding <=> $2::vector)) >= $3
		ORDER BY embedding <=> $2::vector
		LIMIT $4`,
		kbID, embStr, 1.0-threshold, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, content, srcURL, docID string
		var chunkIdx int
		var similarity float64
		rows.Scan(&id, &content, &srcURL, &docID, &chunkIdx, &similarity)
		results = append(results, map[string]interface{}{
			"id": id, "content": content, "source_url": srcURL,
			"doc_id": docID, "chunk_index": chunkIdx, "similarity": similarity,
		})
	}

	if results == nil {
		results = []map[string]interface{}{}
	}
	return results, nil
}

// ── HTTP API ───────────────────────────────────────────────────────────────

func (ve *VectorEngine) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "memory-vector-engine"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := ve.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Store memory with auto-embedding
	r.POST("/agents/:agent_id/memories", func(c *gin.Context) {
		var body struct {
			Content        string                 `json:"content" binding:"required"`
			MemoryType     string                 `json:"memory_type"`
			Key            string                 `json:"key"`
			SessionID      string                 `json:"session_id"`
			Importance     float64                `json:"importance"`
			Metadata       map[string]interface{} `json:"metadata"`
			OrganisationID string                 `json:"organisation_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.MemoryType == "" {
			body.MemoryType = "short_term"
		}
		if body.Importance == 0 {
			body.Importance = 0.5
		}
		id, err := ve.StoreMemoryWithEmbedding(
			c.Request.Context(), c.Param("agent_id"), body.OrganisationID,
			body.Content, body.MemoryType, body.Key, body.SessionID,
			body.Importance, body.Metadata)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id})
	})

	// Semantic search over agent memories
	r.POST("/agents/:agent_id/memories/search", func(c *gin.Context) {
		var body struct {
			Query      string  `json:"query" binding:"required"`
			MemoryType string  `json:"memory_type"`
			Limit      int     `json:"limit"`
			Threshold  float64 `json:"threshold"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.Limit == 0 {
			body.Limit = 10
		}
		if body.Threshold == 0 {
			body.Threshold = 0.7
		}
		results, err := ve.SemanticSearch(
			c.Request.Context(), c.Param("agent_id"), body.Query,
			body.MemoryType, body.Limit, body.Threshold)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"results": results, "count": len(results)})
	})

	// Create knowledge base
	r.POST("/knowledge-bases", func(c *gin.Context) {
		var body struct {
			OrganisationID string `json:"organisation_id" binding:"required"`
			AgentID        string `json:"agent_id"`
			Name           string `json:"name" binding:"required"`
			Description    string `json:"description"`
			EmbeddingModel string `json:"embedding_model"`
			ChunkSize      int    `json:"chunk_size"`
			ChunkOverlap   int    `json:"chunk_overlap"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.EmbeddingModel == "" {
			body.EmbeddingModel = ve.cfg.EmbeddingModel
		}
		if body.ChunkSize == 0 {
			body.ChunkSize = 512
		}
		if body.ChunkOverlap == 0 {
			body.ChunkOverlap = 50
		}

		var id string
		err := ve.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO knowledge_bases
			  (organisation_id, agent_id, name, description, embedding_model, chunk_size, chunk_overlap)
			VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5, $6, $7)
			RETURNING id::text`,
			body.OrganisationID, body.AgentID, body.Name, body.Description,
			body.EmbeddingModel, body.ChunkSize, body.ChunkOverlap).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id})
	})

	// Index a document into a knowledge base
	r.POST("/knowledge-bases/:kb_id/index", func(c *gin.Context) {
		var body struct {
			Content        string `json:"content" binding:"required"`
			SourceURL      string `json:"source_url"`
			DocumentID     string `json:"document_id"`
			OrganisationID string `json:"organisation_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		count, err := ve.IndexDocument(
			c.Request.Context(), c.Param("kb_id"), body.OrganisationID,
			body.Content, body.SourceURL, body.DocumentID)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"chunks_indexed": count})
	})

	// Search knowledge base
	r.POST("/knowledge-bases/:kb_id/search", func(c *gin.Context) {
		var body struct {
			Query     string  `json:"query" binding:"required"`
			Limit     int     `json:"limit"`
			Threshold float64 `json:"threshold"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.Limit == 0 {
			body.Limit = 10
		}
		if body.Threshold == 0 {
			body.Threshold = 0.7
		}
		results, err := ve.SearchKnowledge(c.Request.Context(), c.Param("kb_id"), body.Query, body.Limit, body.Threshold)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"results": results, "count": len(results)})
	})

	return r
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting memory-vector-engine", "port", cfg.Port)

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
		nc, err = natsgo.Connect(cfg.NatsURL, natsgo.MaxReconnects(-1), natsgo.Name("memory-vector-engine"))
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

	ve := New(cfg, db, nc)
	router := ve.setupRouter()
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

func chunkText(text string, chunkSize, overlap int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}
	var chunks []string
	start := 0
	for start < len(text) {
		end := start + chunkSize
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[start:end])
		start += chunkSize - overlap
		if start >= len(text) {
			break
		}
	}
	return chunks
}