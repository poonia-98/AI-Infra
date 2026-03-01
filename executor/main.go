package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gin-gonic/gin"
	natsgo "github.com/nats-io/nats.go"
)

type ContainerStartRequest struct {
	AgentID     string            `json:"agent_id"`
	Image       string            `json:"image"`
	Name        string            `json:"name"`
	CPULimit    float64           `json:"cpu_limit"`
	MemoryLimit string            `json:"memory_limit"`
	EnvVars     map[string]string `json:"env_vars"`
	Labels      map[string]string `json:"labels"`
}

type ContainerStopRequest struct {
	AgentID     string `json:"agent_id"`
	ContainerID string `json:"container_id"`
}

type ContainerResponse struct {
	Success       bool   `json:"success"`
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	Error         string `json:"error,omitempty"`
}

type lineWriter struct {
	buf    strings.Builder
	onLine func(line, stream string)
	stream string
}

func (w *lineWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			w.onLine(w.buf.String(), w.stream)
			w.buf.Reset()
		} else {
			w.buf.WriteByte(b)
		}
	}
	return len(p), nil
}

func detectLevel(line string) string {
	u := strings.ToUpper(line)
	switch {
	case strings.Contains(u, "ERROR") || strings.Contains(u, "FATAL") || strings.Contains(u, "EXCEPTION"):
		return "error"
	case strings.Contains(u, "WARN"):
		return "warn"
	case strings.Contains(u, "DEBUG"):
		return "debug"
	default:
		return "info"
	}
}

type LogStreamer struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewLogStreamer() *LogStreamer {
	return &LogStreamer{cancels: make(map[string]context.CancelFunc)}
}

func (ls *LogStreamer) Start(agentID, containerID string, dc *client.Client, nc *natsgo.Conn, cpURL string) {
	ls.mu.Lock()
	if cancel, exists := ls.cancels[agentID]; exists {
		cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ls.cancels[agentID] = cancel
	ls.mu.Unlock()

	go func() {
		defer func() {
			ls.mu.Lock()
			delete(ls.cancels, agentID)
			ls.mu.Unlock()
		}()

		reader, err := dc.ContainerLogs(ctx, containerID, container.LogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Follow:     true,
			Timestamps: false,
			Since:      "0s",
		})
		if err != nil {
			log.Printf("[executor] ContainerLogs error %s: %v", containerID[:12], err)
			return
		}
		defer reader.Close()

		emit := func(line, stream string) {
			line = strings.TrimSpace(line)
			if line == "" {
				return
			}
			level := detectLevel(line)
			if stream == "stderr" && level == "info" {
				level = "warn"
			}
			payload := map[string]interface{}{
				"agent_id":  agentID,
				"message":   line,
				"level":     level,
				"source":    "container",
				"timestamp": time.Now().UTC().Format(time.RFC3339),
			}
			data, _ := json.Marshal(payload)
			nc.Publish("logs.stream", data)
			go persistLog(cpURL, agentID, line, level, "container")
		}

		stdoutW := &lineWriter{onLine: emit, stream: "stdout"}
		stderrW := &lineWriter{onLine: emit, stream: "stderr"}

		if _, err := stdcopy.StdCopy(stdoutW, stderrW, reader); err != nil {
			if ctx.Err() == nil {
				log.Printf("[executor] stdcopy error %s: %v", containerID[:12], err)
			}
		}

		if stdoutW.buf.Len() > 0 {
			emit(stdoutW.buf.String(), "stdout")
		}
		if stderrW.buf.Len() > 0 {
			emit(stderrW.buf.String(), "stderr")
		}

		inspect, err := dc.ContainerInspect(context.Background(), containerID)
		if err != nil {
			return
		}
		exitCode := 0
		if inspect.State != nil {
			exitCode = inspect.State.ExitCode
		}
		evType := "agent.completed"
		if exitCode != 0 {
			evType = "agent.failed"
		}
		edata, _ := json.Marshal(map[string]interface{}{
			"event_type": evType, "agent_id": agentID,
			"container_id": containerID, "exit_code": exitCode,
			"source": "executor", "level": map[bool]string{true: "warn", false: "info"}[exitCode != 0],
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		nc.Publish("agents.events", edata)
		log.Printf("[executor] Container %s exited (code=%d)", containerID[:12], exitCode)
	}()
}

func (ls *LogStreamer) Stop(agentID string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if cancel, exists := ls.cancels[agentID]; exists {
		cancel()
		delete(ls.cancels, agentID)
	}
}

func persistLog(cpURL, agentID, message, level, source string) {
	data, _ := json.Marshal(map[string]interface{}{
		"agent_id": agentID, "message": message,
		"level": level, "source": source,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", cpURL+"/api/v1/logs", bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv("CONTROL_PLANE_API_KEY"); apiKey != "" {
		req.Header.Set("X-Service-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func parseMemoryLimit(limit string) int64 {
	if len(limit) < 2 {
		return 512 * 1024 * 1024
	}
	suffix := limit[len(limit)-1]
	var mul int64 = 1
	switch suffix {
	case 'm', 'M':
		mul = 1024 * 1024
	case 'g', 'G':
		mul = 1024 * 1024 * 1024
	case 'k', 'K':
		mul = 1024
	default:
		return 512 * 1024 * 1024
	}
	var val int64
	fmt.Sscanf(limit[:len(limit)-1], "%d", &val)
	return val * mul
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func requireServiceAuth(expectedKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		got := c.GetHeader("X-Service-API-Key")
		if expectedKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(expectedKey)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

func main() {
	natsURL := getEnv("NATS_URL", "nats://nats:4222")
	cpURL   := getEnv("CONTROL_PLANE_URL", "http://backend:8000")
	network := getEnv("AGENT_NETWORK", "platform_network")
	serviceAPIKey := getEnv("CONTROL_PLANE_API_KEY", "")

	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[executor] Docker client error: %v", err)
	}
	defer dc.Close()

	ctx10, c10 := context.WithTimeout(context.Background(), 10*time.Second)
	info, err := dc.Info(ctx10)
	c10()
	if err != nil {
		log.Fatalf("[executor] Cannot reach Docker: %v", err)
	}
	log.Printf("[executor] Docker %s / API %s", info.ServerVersion, dc.ClientVersion())

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(natsURL, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second))
		if err == nil {
			break
		}
		log.Printf("[executor] Waiting for NATS %d/30...", i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("[executor] NATS failed: %v", err)
	}
	defer nc.Close()
	log.Printf("[executor] NATS connected")

	streamer := NewLogStreamer()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "executor"})
	})

	protected := r.Group("/")
	protected.Use(requireServiceAuth(serviceAPIKey))

	protected.POST("/containers/start", func(c *gin.Context) {
		var req ContainerStartRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, ContainerResponse{Success: false, Error: err.Error()})
			return
		}

		env := []string{
			fmt.Sprintf("CONTROL_PLANE_URL=%s", cpURL),
			fmt.Sprintf("NATS_URL=%s", natsURL),
		}
		for k, v := range req.EnvVars {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}

		labels := map[string]string{
			"platform.managed":  "true",
			"platform.agent_id": req.AgentID,
		}
		for k, v := range req.Labels {
			labels[k] = v
		}

		ctx := context.Background()

		if _, _, e := dc.ImageInspectWithRaw(ctx, req.Image); e != nil {
			log.Printf("[executor] Pulling %s...", req.Image)
			r2, e2 := dc.ImagePull(ctx, req.Image, image.PullOptions{})
			if e2 != nil {
				log.Printf("[executor] Pull warning: %v", e2)
			} else {
				io.Copy(io.Discard, r2)
				r2.Close()
			}
		}

		resp, err := dc.ContainerCreate(ctx,
			&container.Config{Image: req.Image, Env: env, Labels: labels},
			&container.HostConfig{
				NetworkMode: container.NetworkMode(network),
				Resources: container.Resources{
					Memory:   parseMemoryLimit(req.MemoryLimit),
					CPUQuota: int64(req.CPULimit * 100000),
				},
				RestartPolicy: container.RestartPolicy{Name: "no"},
			},
			nil, nil, req.Name,
		)
		if err != nil {
			c.JSON(500, ContainerResponse{Success: false, Error: err.Error()})
			return
		}

		if err := dc.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
			dc.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			c.JSON(500, ContainerResponse{Success: false, Error: err.Error()})
			return
		}

		streamer.Start(req.AgentID, resp.ID, dc, nc, cpURL)

		edata, _ := json.Marshal(map[string]interface{}{
			"event_type": "agent.started", "agent_id": req.AgentID,
			"container_id": resp.ID, "container_name": req.Name,
			"source": "executor", "level": "info",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		nc.Publish("agents.events", edata)

		log.Printf("[executor] Started %s for agent %s", resp.ID[:12], req.AgentID)
		c.JSON(200, ContainerResponse{Success: true, ContainerID: resp.ID, ContainerName: req.Name})
	})

	protected.POST("/containers/stop", func(c *gin.Context) {
		var req ContainerStopRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, ContainerResponse{Success: false, Error: err.Error()})
			return
		}

		cid := req.ContainerID
		if cid == "" {
			ctx := context.Background()
			f := filters.NewArgs()
			f.Add("label", fmt.Sprintf("platform.agent_id=%s", req.AgentID))
			ctrs, err := dc.ContainerList(ctx, container.ListOptions{Filters: f})
			if err != nil || len(ctrs) == 0 {
				c.JSON(404, ContainerResponse{Success: false, Error: "container not found"})
				return
			}
			cid = ctrs[0].ID
		}

		streamer.Stop(req.AgentID)
		timeout := 10
		ctx := context.Background()
		dc.ContainerStop(ctx, cid, container.StopOptions{Timeout: &timeout})
		dc.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true})

		edata, _ := json.Marshal(map[string]interface{}{
			"event_type": "agent.stopped", "agent_id": req.AgentID,
			"container_id": cid, "source": "executor", "level": "info",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		nc.Publish("agents.events", edata)

		log.Printf("[executor] Stopped %s", cid[:12])
		c.JSON(200, ContainerResponse{Success: true, ContainerID: cid})
	})

	protected.GET("/containers", func(c *gin.Context) {
		ctx := context.Background()
		f := filters.NewArgs()
		f.Add("label", "platform.managed=true")
		ctrs, err := dc.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		out := make([]map[string]interface{}, 0, len(ctrs))
		for _, ctr := range ctrs {
			out = append(out, map[string]interface{}{
				"id": ctr.ID[:12], "name": ctr.Names,
				"image": ctr.Image, "status": ctr.Status,
				"state": ctr.State, "agent_id": ctr.Labels["platform.agent_id"],
			})
		}
		c.JSON(200, out)
	})

	protected.GET("/containers/:agent_id/stats", func(c *gin.Context) {
		agentID := c.Param("agent_id")
		ctx := context.Background()
		f := filters.NewArgs()
		f.Add("label", fmt.Sprintf("platform.agent_id=%s", agentID))
		ctrs, err := dc.ContainerList(ctx, container.ListOptions{Filters: f})
		if err != nil || len(ctrs) == 0 {
			c.JSON(404, gin.H{"error": "container not found"})
			return
		}
		sr, err := dc.ContainerStats(ctx, ctrs[0].ID, false)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer sr.Body.Close()
		var stats container.StatsResponse
		if err := json.NewDecoder(sr.Body).Decode(&stats); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		cpuD := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
		sysD := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
		cpu := 0.0
		if sysD > 0 {
			n := float64(stats.CPUStats.OnlineCPUs)
			if n == 0 { n = 1 }
			cpu = (cpuD / sysD) * n * 100.0
		}
		memMB := float64(stats.MemoryStats.Usage) / 1024 / 1024
		limMB := float64(stats.MemoryStats.Limit) / 1024 / 1024
		pct := 0.0
		if limMB > 0 { pct = memMB / limMB * 100 }
		c.JSON(200, gin.H{"cpu_percent": cpu, "memory_usage_mb": memMB, "memory_limit_mb": limMB, "memory_percent": pct})
	})

	port := getEnv("PORT", "8081")
	nodeID := getEnv("NODE_ID", "executor-default")
	hostname, _ := os.Hostname()

	// ── Register heartbeat with control plane ──────────────────────────
	go func() {
		hbClient := &http.Client{Timeout: 5 * time.Second}
		sendHeartbeat := func() {
			payload, _ := json.Marshal(map[string]interface{}{
				"node_id":  nodeID,
				"hostname": hostname,
				"address":  "http://executor:" + port,
				"port":     8081,
			})
			req, err := http.NewRequest("POST", cpURL+"/api/v1/nodes/heartbeat", bytes.NewReader(payload))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if serviceAPIKey != "" {
				req.Header.Set("X-Service-API-Key", serviceAPIKey)
			}
			resp, err := hbClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
		sendHeartbeat()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sendHeartbeat()
		}
	}()

	log.Printf("[executor] Listening on :%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("[executor] Server error: %v", err)
	}
}
