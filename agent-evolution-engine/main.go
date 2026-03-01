package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
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
	DatabaseURL string
	NatsURL     string
	BackendURL  string
	Port        string
	EvalInterval time.Duration
}

func loadConfig() Config {
	interval, _ := time.ParseDuration(getEnv("EVAL_INTERVAL", "5m"))
	return Config{
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@postgres:5432/agentdb?sslmode=disable"),
		NatsURL:      getEnv("NATS_URL", "nats://nats:4222"),
		BackendURL:   getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		Port:         getEnv("PORT", "8103"),
		EvalInterval: interval,
	}
}

var (
	agentsEvolved = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_evolution_agents_evolved_total"})
	agentsCloned  = prometheus.NewCounter(prometheus.CounterOpts{Name: "platform_evolution_agents_cloned_total"})
	bestFitness   = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "platform_evolution_best_fitness"}, []string{"experiment_id"})
)

func init() {
	prometheus.MustRegister(agentsEvolved, agentsCloned, bestFitness)
}

// ── Genome types ───────────────────────────────────────────────────────────

type Genome struct {
	CPULimit      float64            `json:"cpu_limit"`
	MemoryLimit   string             `json:"memory_limit"`
	BatchSize     int                `json:"batch_size,omitempty"`
	Concurrency   int                `json:"concurrency,omitempty"`
	RetryPolicy   string             `json:"retry_policy,omitempty"`
	TimeoutSec    int                `json:"timeout_seconds,omitempty"`
	EnvOverrides  map[string]string  `json:"env_overrides,omitempty"`
	ConfigParams  map[string]interface{} `json:"config_params,omitempty"`
}

type EvolutionEngine struct {
	cfg    Config
	db     *sql.DB
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
	rng    *rand.Rand
}

func New(cfg Config, db *sql.DB, nc *natsgo.Conn) *EvolutionEngine {
	return &EvolutionEngine{
		cfg:    cfg,
		db:     db,
		nc:     nc,
		http:   &http.Client{Timeout: 30 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ── Evolution loop ─────────────────────────────────────────────────────────

func (e *EvolutionEngine) RunEvolutionLoop(ctx context.Context) {
	ticker := time.NewTicker(e.cfg.EvalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.processExperiments(ctx)
		}
	}
}

func (e *EvolutionEngine) processExperiments(ctx context.Context) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id::text, base_agent_id::text, organisation_id::text,
		       strategy, objective, config, current_generation
		FROM evolution_experiments WHERE status='active'`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, baseAgentID, orgID, strategy, objective string
		var configJSON []byte
		var generation int
		rows.Scan(&id, &baseAgentID, &orgID, &strategy, &objective, &configJSON, &generation)

		var cfg map[string]interface{}
		json.Unmarshal(configJSON, &cfg)

		e.evolveGeneration(ctx, id, baseAgentID, orgID, strategy, objective, cfg, generation)
	}
}

func (e *EvolutionEngine) evolveGeneration(ctx context.Context, experimentID, baseAgentID, orgID,
	strategy, objective string, cfg map[string]interface{}, generation int) {

	// Get current population fitness scores
	population, err := e.evaluatePopulation(ctx, experimentID)
	if err != nil || len(population) == 0 {
		// Bootstrap initial population
		e.bootstrapPopulation(ctx, experimentID, baseAgentID, orgID, cfg)
		return
	}

	// Sort by fitness
	sortByFitness(population)

	// Update best in experiment
	if len(population) > 0 {
		best := population[len(population)-1]
		e.db.ExecContext(ctx, `
			UPDATE evolution_experiments
			SET best_fitness=$1, best_agent_id=$2::uuid,
			    current_generation=current_generation+1, updated_at=NOW()
			WHERE id=$3::uuid`, best.Fitness, best.AgentID, experimentID)
		bestFitness.With(prometheus.Labels{"experiment_id": experimentID}).Set(best.Fitness)
	}

	populationSize := getIntCfg(cfg, "population_size", 10)
	mutationRate := getFloatCfg(cfg, "mutation_rate", 0.2)
	eliteCount := max(1, populationSize/5)

	// Elitism: keep top N
	elites := population[len(population)-eliteCount:]

	// Create new generation
	newGenAgents := []string{}
	for _, elite := range elites {
		newGenAgents = append(newGenAgents, elite.AgentID)
	}

	// Fill rest with mutations and crossovers
	for len(newGenAgents) < populationSize {
		parent := elites[e.rng.Intn(len(elites))]
		genome, err := e.getGenome(ctx, parent.AgentID)
		if err != nil {
			continue
		}

		var newGenome Genome
		if e.rng.Float64() < mutationRate {
			newGenome = e.mutate(genome)
		} else if len(elites) > 1 {
			parent2 := elites[e.rng.Intn(len(elites))]
			genome2, _ := e.getGenome(ctx, parent2.AgentID)
			newGenome = e.crossover(genome, genome2)
		} else {
			newGenome = e.mutate(genome)
		}

		cloneID, err := e.cloneAgent(ctx, parent.AgentID, orgID, experimentID, newGenome, "mutated")
		if err != nil {
			e.logger.Error("clone failed", "error", err)
			continue
		}
		newGenAgents = append(newGenAgents, cloneID)
		agentsEvolved.Inc()
	}

	populationJSON, _ := json.Marshal(newGenAgents)
	e.db.ExecContext(ctx, `
		UPDATE evolution_experiments SET population=$1::uuid[], updated_at=NOW()
		WHERE id=$2::uuid`, fmt.Sprintf("{%s}", joinIDs(newGenAgents)), experimentID)

	e.logger.Info("evolution generation completed",
		"experiment_id", experimentID, "generation", generation,
		"population_size", len(newGenAgents))
}

func (e *EvolutionEngine) bootstrapPopulation(ctx context.Context, experimentID, baseAgentID, orgID string, cfg map[string]interface{}) {
	populationSize := getIntCfg(cfg, "population_size", 10)
	baseGenome, err := e.getGenome(ctx, baseAgentID)
	if err != nil {
		baseGenome = defaultGenome()
	}

	var population []string
	population = append(population, baseAgentID)

	for i := 1; i < populationSize; i++ {
		mutated := e.mutate(baseGenome)
		cloneID, err := e.cloneAgent(ctx, baseAgentID, orgID, experimentID, mutated, "mutated")
		if err != nil {
			continue
		}
		population = append(population, cloneID)
	}

	e.db.ExecContext(ctx, `
		UPDATE evolution_experiments
		SET population=$1::uuid[], current_generation=1, updated_at=NOW()
		WHERE id=$2::uuid`,
		fmt.Sprintf("{%s}", joinIDs(population)), experimentID)

	e.logger.Info("population bootstrapped", "experiment_id", experimentID, "size", len(population))
}

// ── Genetic operators ──────────────────────────────────────────────────────

func (e *EvolutionEngine) mutate(g Genome) Genome {
	m := g // copy
	field := e.rng.Intn(4)
	switch field {
	case 0:
		delta := (e.rng.Float64() - 0.5) * 0.5
		m.CPULimit = math.Max(0.1, math.Min(8.0, g.CPULimit+delta))
	case 1:
		mems := []string{"128m", "256m", "512m", "1g", "2g", "4g"}
		m.MemoryLimit = mems[e.rng.Intn(len(mems))]
	case 2:
		if g.BatchSize > 0 {
			delta := e.rng.Intn(11) - 5
			m.BatchSize = max(1, g.BatchSize+delta)
		}
	case 3:
		if g.TimeoutSec > 0 {
			delta := (e.rng.Intn(61) - 30)
			m.TimeoutSec = max(10, g.TimeoutSec+delta)
		}
	}
	return m
}

func (e *EvolutionEngine) crossover(a, b Genome) Genome {
	child := Genome{
		EnvOverrides: map[string]string{},
		ConfigParams: map[string]interface{}{},
	}
	if e.rng.Float64() < 0.5 {
		child.CPULimit = a.CPULimit
	} else {
		child.CPULimit = b.CPULimit
	}
	if e.rng.Float64() < 0.5 {
		child.MemoryLimit = a.MemoryLimit
	} else {
		child.MemoryLimit = b.MemoryLimit
	}
	if e.rng.Float64() < 0.5 {
		child.BatchSize = a.BatchSize
	} else {
		child.BatchSize = b.BatchSize
	}
	if e.rng.Float64() < 0.5 {
		child.TimeoutSec = a.TimeoutSec
	} else {
		child.TimeoutSec = b.TimeoutSec
	}
	// Merge env overrides
	for k, v := range a.EnvOverrides {
		child.EnvOverrides[k] = v
	}
	for k, v := range b.EnvOverrides {
		if _, exists := child.EnvOverrides[k]; !exists || e.rng.Float64() < 0.5 {
			child.EnvOverrides[k] = v
		}
	}
	return child
}

// ── Fitness evaluation ─────────────────────────────────────────────────────

type IndividualFitness struct {
	AgentID string
	Fitness float64
}

func (e *EvolutionEngine) evaluatePopulation(ctx context.Context, experimentID string) ([]IndividualFitness, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT ca.clone_agent_id::text, COALESCE(ag.fitness_score, 0)
		FROM cloned_agents ca
		LEFT JOIN agent_genomes ag ON ag.agent_id=ca.clone_agent_id
		WHERE ca.experiment_id=$1::uuid`, experimentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var population []IndividualFitness
	for rows.Next() {
		var agentID string
		var fitness float64
		rows.Scan(&agentID, &fitness)
		population = append(population, IndividualFitness{AgentID: agentID, Fitness: fitness})
	}
	return population, nil
}

func (e *EvolutionEngine) computeFitness(ctx context.Context, agentID, objective string) float64 {
	// Compute fitness from recent execution metrics
	row := e.db.QueryRowContext(ctx, `
		SELECT
		  COALESCE(COUNT(*) FILTER (WHERE status='completed')::float / NULLIF(COUNT(*),0), 0) as success_rate,
		  COALESCE(AVG(duration_ms), 9999999) as avg_duration_ms,
		  COALESCE(AVG(exit_code), 1) as avg_exit_code
		FROM executions
		WHERE agent_id=$1::uuid
		  AND created_at >= NOW() - INTERVAL '1 hour'`, agentID)

	var successRate, avgDuration, avgExitCode float64
	row.Scan(&successRate, &avgDuration, &avgExitCode)

	switch objective {
	case "minimize_latency":
		return successRate * (1000.0 / math.Max(avgDuration, 1))
	case "maximize_throughput":
		return successRate * 100
	case "minimize_cost":
		return successRate / math.Max(avgDuration/1000, 0.001)
	default:
		return successRate * 100
	}
}

// ── Clone agent via backend API ────────────────────────────────────────────

func (e *EvolutionEngine) cloneAgent(ctx context.Context, sourceAgentID, orgID, experimentID string, genome Genome, cloneType string) (string, error) {
	// Get source agent
	var name, image, agentType string
	var configJSON, envJSON []byte
	err := e.db.QueryRowContext(ctx, `
		SELECT name, image, agent_type, config, env_vars
		FROM agents WHERE id=$1::uuid`, sourceAgentID).
		Scan(&name, &image, &agentType, &configJSON, &envJSON)
	if err != nil {
		return "", err
	}

	var config map[string]interface{}
	var envVars map[string]string
	json.Unmarshal(configJSON, &config)
	json.Unmarshal(envJSON, &envVars)

	// Apply genome mutations
	for k, v := range genome.EnvOverrides {
		envVars[k] = v
	}
	for k, v := range genome.ConfigParams {
		config[k] = v
	}

	configOut, _ := json.Marshal(config)
	envOut, _ := json.Marshal(envVars)

	cloneName := fmt.Sprintf("%s-evo-%d", name, time.Now().Unix())
	payload, _ := json.Marshal(map[string]interface{}{
		"name":            cloneName,
		"image":           image,
		"agent_type":      agentType,
		"config":          json.RawMessage(configOut),
		"env_vars":        json.RawMessage(envOut),
		"organisation_id": orgID,
		"cpu_limit":       genome.CPULimit,
		"memory_limit":    genome.MemoryLimit,
		"labels": map[string]string{
			"evolved_from":   sourceAgentID,
			"experiment_id":  experimentID,
			"clone_type":     cloneType,
		},
	})

	req, err := http.NewRequestWithContext(ctx, "POST",
		e.cfg.BackendURL+"/api/v1/agents", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor-Type", "evolution-engine")

	resp, err := e.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("backend returned %d", resp.StatusCode)
	}

	var result struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	// Record genome and clone relationship
	genomeJSON, _ := json.Marshal(genome)
	mutations, _ := json.Marshal([]map[string]interface{}{
		{"type": cloneType, "from": sourceAgentID, "genome": genome},
	})

	var genomeID string
	e.db.QueryRowContext(ctx, `
		INSERT INTO agent_genomes (agent_id, organisation_id, genome, generation, parent_ids, mutation_log)
		SELECT $1::uuid, organisation_id, $2::jsonb, 1, ARRAY[$3::uuid], $4::jsonb
		FROM agents WHERE id=$3::uuid
		RETURNING id::text`,
		result.ID, string(genomeJSON), sourceAgentID, string(mutations)).Scan(&genomeID)

	expID := experimentID
	e.db.ExecContext(ctx, `
		INSERT INTO cloned_agents (source_agent_id, clone_agent_id, organisation_id, experiment_id, clone_type)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5)`,
		sourceAgentID, result.ID, orgID, expID, cloneType)

	agentsCloned.Inc()
	e.logger.Info("agent cloned", "source", sourceAgentID, "clone", result.ID, "type", cloneType)
	return result.ID, nil
}

func (e *EvolutionEngine) getGenome(ctx context.Context, agentID string) (Genome, error) {
	var genomeJSON []byte
	err := e.db.QueryRowContext(ctx, `
		SELECT genome FROM agent_genomes WHERE agent_id=$1::uuid ORDER BY created_at DESC LIMIT 1`, agentID).Scan(&genomeJSON)
	if err != nil {
		// Derive genome from agent config
		var cpuLimit float64
		var memLimit string
		e.db.QueryRowContext(ctx, `SELECT cpu_limit, memory_limit FROM agents WHERE id=$1::uuid`, agentID).Scan(&cpuLimit, &memLimit)
		return Genome{CPULimit: cpuLimit, MemoryLimit: memLimit, TimeoutSec: 300}, nil
	}
	var g Genome
	json.Unmarshal(genomeJSON, &g)
	return g, nil
}

// ── HTTP API ───────────────────────────────────────────────────────────────

func (e *EvolutionEngine) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "agent-evolution-engine"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if err := e.db.PingContext(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Create evolution experiment
	r.POST("/experiments", func(c *gin.Context) {
		var body struct {
			OrganisationID string                 `json:"organisation_id" binding:"required"`
			Name           string                 `json:"name" binding:"required"`
			Description    string                 `json:"description"`
			BaseAgentID    string                 `json:"base_agent_id" binding:"required"`
			Strategy       string                 `json:"strategy"`
			Objective      string                 `json:"objective"`
			Config         map[string]interface{} `json:"config"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.Strategy == "" {
			body.Strategy = "genetic"
		}
		if body.Objective == "" {
			body.Objective = "minimize_latency"
		}
		cfgJSON, _ := json.Marshal(body.Config)

		var id string
		err := e.db.QueryRowContext(c.Request.Context(), `
			INSERT INTO evolution_experiments
			  (organisation_id, name, description, base_agent_id, strategy, objective, config)
			VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7::jsonb)
			RETURNING id::text`,
			body.OrganisationID, body.Name, body.Description, body.BaseAgentID,
			body.Strategy, body.Objective, string(cfgJSON)).Scan(&id)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"id": id, "status": "active"})
	})

	// Clone an agent
	r.POST("/agents/:agent_id/clone", func(c *gin.Context) {
		var body struct {
			OrganisationID string  `json:"organisation_id" binding:"required"`
			ExperimentID   string  `json:"experiment_id"`
			CPULimit       float64 `json:"cpu_limit"`
			MemoryLimit    string  `json:"memory_limit"`
			MutationRate   float64 `json:"mutation_rate"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}

		baseGenome, _ := e.getGenome(c.Request.Context(), c.Param("agent_id"))
		if body.CPULimit > 0 {
			baseGenome.CPULimit = body.CPULimit
		}
		if body.MemoryLimit != "" {
			baseGenome.MemoryLimit = body.MemoryLimit
		}

		genome := baseGenome
		if body.MutationRate > 0 && e.rng.Float64() < body.MutationRate {
			genome = e.mutate(baseGenome)
		}

		cloneID, err := e.cloneAgent(c.Request.Context(),
			c.Param("agent_id"), body.OrganisationID, body.ExperimentID, genome, "exact")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"clone_id": cloneID, "source_agent_id": c.Param("agent_id")})
	})

	// Get experiment status
	r.GET("/experiments/:id", func(c *gin.Context) {
		row := e.db.QueryRowContext(c.Request.Context(), `
			SELECT id::text, name, status, strategy, objective,
			       current_generation, best_fitness, best_agent_id::text,
			       started_at, completed_at
			FROM evolution_experiments WHERE id=$1::uuid`, c.Param("id"))
		var id, name, status, strategy, objective, bestAgentID string
		var generation int
		var bestFitnessVal float64
		var startedAt time.Time
		var completedAt *time.Time
		if err := row.Scan(&id, &name, &status, &strategy, &objective, &generation,
			&bestFitnessVal, &bestAgentID, &startedAt, &completedAt); err != nil {
			c.JSON(404, gin.H{"error": "not found"})
			return
		}
		c.JSON(200, gin.H{
			"id": id, "name": name, "status": status,
			"strategy": strategy, "objective": objective,
			"current_generation": generation, "best_fitness": bestFitnessVal,
			"best_agent_id": bestAgentID, "started_at": startedAt, "completed_at": completedAt,
		})
	})

	// Update fitness score for an agent (called after evaluation)
	r.POST("/agents/:agent_id/fitness", func(c *gin.Context) {
		var body struct {
			FitnessScore float64 `json:"fitness_score" binding:"required"`
			ExperimentID string  `json:"experiment_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		e.db.ExecContext(c.Request.Context(), `
			INSERT INTO agent_genomes (agent_id, fitness_score)
			VALUES ($1::uuid, $2)
			ON CONFLICT (agent_id) DO UPDATE SET
			  fitness_score=$2,
			  fitness_history=fitness_history || jsonb_build_array($2),
			  updated_at=NOW()
			WHERE agent_genomes.agent_id=$1::uuid`,
			c.Param("agent_id"), body.FitnessScore)

		if body.ExperimentID != "" {
			e.db.ExecContext(c.Request.Context(), `
				UPDATE cloned_agents SET fitness_score=$1, evaluated=TRUE
				WHERE clone_agent_id=$2::uuid AND experiment_id=$3::uuid`,
				body.FitnessScore, c.Param("agent_id"), body.ExperimentID)
		}
		c.JSON(200, gin.H{"status": "updated"})
	})

	return r
}

// ── Main ───────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting agent-evolution-engine", "port", cfg.Port)

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
			natsgo.Name("agent-evolution-engine"))
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

	engine := New(cfg, db, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.RunEvolutionLoop(ctx)

	router := engine.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

// ── Helpers ────────────────────────────────────────────────────────────────

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func getIntCfg(cfg map[string]interface{}, key string, def int) int {
	if v, ok := cfg[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return def
}
func getFloatCfg(cfg map[string]interface{}, key string, def float64) float64 {
	if v, ok := cfg[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}
func defaultGenome() Genome {
	return Genome{CPULimit: 1.0, MemoryLimit: "512m", TimeoutSec: 300, BatchSize: 10, Concurrency: 1}
}
func sortByFitness(pop []IndividualFitness) {
	for i := 1; i < len(pop); i++ {
		for j := i; j > 0 && pop[j].Fitness < pop[j-1].Fitness; j-- {
			pop[j], pop[j-1] = pop[j-1], pop[j]
		}
	}
}
func joinIDs(ids []string) string {
	result := ""
	for i, id := range ids {
		if i > 0 {
			result += ","
		}
		result += id
	}
	return result
}