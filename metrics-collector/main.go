package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	agentCPU = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "agent_cpu_percent", Help: "CPU %"},
		[]string{"agent_id", "container_id"},
	)
	agentMem = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "agent_memory_mb", Help: "Memory MB"},
		[]string{"agent_id", "container_id"},
	)
	agentMemPct = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "agent_memory_percent", Help: "Memory %"},
		[]string{"agent_id", "container_id"},
	)
	agentStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "agent_status", Help: "1=running"},
		[]string{"agent_id"},
	)
	platformTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_total_agents", Help: "Total managed agents",
	})
	platformRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_running_agents", Help: "Running agents",
	})
	metricsPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "metrics_published_total", Help: "Metrics published",
	})
)

func init() {
	prometheus.MustRegister(agentCPU, agentMem, agentMemPct,
		agentStatus, platformTotal, platformRunning, metricsPublished)
}

type Collector struct {
	dc    *client.Client
	nc    *natsgo.Conn
	cpURL string
	http  *http.Client
}

func (c *Collector) collect(ctx context.Context) {
	f := filters.NewArgs()
	f.Add("label", "platform.managed=true")
	ctrs, err := c.dc.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		log.Printf("[metrics] ContainerList error: %v", err)
		return
	}

	platformTotal.Set(float64(len(ctrs)))
	running := 0

	for _, ctr := range ctrs {
		agentID := ctr.Labels["platform.agent_id"]
		cid12   := ctr.ID[:12]

		if ctr.State != "running" {
			agentStatus.WithLabelValues(agentID).Set(0)
			continue
		}
		running++
		agentStatus.WithLabelValues(agentID).Set(1)

		sr, err := c.dc.ContainerStats(ctx, ctr.ID, false)
		if err != nil {
			log.Printf("[metrics] ContainerStats %s: %v", cid12, err)
			continue
		}
		var stats container.StatsResponse
		decErr := json.NewDecoder(sr.Body).Decode(&stats)
		sr.Body.Close()
		if decErr != nil {
			continue
		}

		cpuD := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
		sysD := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
		cpu  := 0.0
		if sysD > 0 {
			n := float64(stats.CPUStats.OnlineCPUs)
			if n == 0 { n = 1 }
			cpu = (cpuD / sysD) * n * 100.0
		}

		memMB  := float64(stats.MemoryStats.Usage) / 1024 / 1024
		limMB  := float64(stats.MemoryStats.Limit)  / 1024 / 1024
		memPct := 0.0
		if limMB > 0 { memPct = memMB / limMB * 100 }

		var netRxMB, netTxMB float64
		for _, v := range stats.Networks {
			netRxMB += float64(v.RxBytes) / 1024 / 1024
			netTxMB += float64(v.TxBytes) / 1024 / 1024
		}

		agentCPU.WithLabelValues(agentID, cid12).Set(cpu)
		agentMem.WithLabelValues(agentID, cid12).Set(memMB)
		agentMemPct.WithLabelValues(agentID, cid12).Set(memPct)

		snap, _ := json.Marshal(map[string]interface{}{
			"agent_id": agentID, "container_id": cid12,
			"cpu_percent": cpu, "memory_mb": memMB,
			"memory_percent": memPct,
			"net_rx_mb": netRxMB, "net_tx_mb": netTxMB,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
		c.nc.Publish("metrics.stream", snap)
		metricsPublished.Add(5)

		go c.persist(agentID, cpu, memMB, memPct, netRxMB, netTxMB)
	}

	platformRunning.Set(float64(running))
}

func (c *Collector) persist(agentID string, cpu, memMB, memPct, netRx, netTx float64) {
	metrics := []map[string]interface{}{
		{"agent_id": agentID, "metric_name": "cpu_percent",    "metric_value": cpu},
		{"agent_id": agentID, "metric_name": "memory_mb",      "metric_value": memMB},
		{"agent_id": agentID, "metric_name": "memory_percent", "metric_value": memPct},
		{"agent_id": agentID, "metric_name": "net_rx_mb",      "metric_value": netRx},
		{"agent_id": agentID, "metric_name": "net_tx_mb",      "metric_value": netTx},
	}
	for _, m := range metrics {
		data, _ := json.Marshal(m)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", c.cpURL+"/api/v1/metrics", bytes.NewReader(data))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	natsURL := getEnv("NATS_URL", "nats://nats:4222")
	cpURL   := getEnv("CONTROL_PLANE_URL", "http://backend:8000")

	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("[metrics] Docker client error: %v", err)
	}
	defer dc.Close()

	ctx10, c10 := context.WithTimeout(context.Background(), 10*time.Second)
	info, err := dc.Info(ctx10)
	c10()
	if err != nil {
		log.Fatalf("[metrics] Cannot reach Docker: %v", err)
	}
	log.Printf("[metrics] Docker %s / API %s", info.ServerVersion, dc.ClientVersion())

	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(natsURL, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second))
		if err == nil {
			break
		}
		log.Printf("[metrics] Waiting for NATS %d/30...", i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("[metrics] NATS failed: %v", err)
	}
	defer nc.Close()
	log.Printf("[metrics] NATS connected")

	col := &Collector{
		dc: dc, nc: nc, cpURL: cpURL,
		http: &http.Client{Timeout: 10 * time.Second},
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		col.collect(ctx)
		cancel()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			col.collect(ctx)
			cancel()
		}
	}()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health",  func(c *gin.Context) { c.JSON(200, gin.H{"status": "healthy"}) })
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	port := getEnv("PORT", "8083")
	log.Printf("[metrics] Listening on :%s", port)
	if err := r.Run(":" + port); err != nil {
		log.Fatalf("[metrics] Server error: %v", err)
	}
}