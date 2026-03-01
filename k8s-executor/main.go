package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	natsgo "github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var _ io.Reader

// ── Config ────────────────────────────────────────────────────────────────────

type Config struct {
	Port             string
	NatsURL          string
	ControlPlaneURL  string
	ControlPlaneAPIKey string
	DefaultNamespace string
	PodTTLSeconds    int
	KubeconfigPath   string // empty = in-cluster
	NodeID           string
}

func loadConfig() Config {
	ttl, _ := strconv.Atoi(getEnv("POD_TTL_SECONDS", "3600"))
	return Config{
		Port:             getEnv("PORT", "8091"),
		NatsURL:          getEnv("NATS_URL", "nats://nats:4222"),
		ControlPlaneURL:  getEnv("CONTROL_PLANE_URL", "http://backend:8000"),
		ControlPlaneAPIKey: getEnv("CONTROL_PLANE_API_KEY", ""),
		DefaultNamespace: getEnv("K8S_NAMESPACE", "ai-agents"),
		PodTTLSeconds:    ttl,
		KubeconfigPath:   getEnv("KUBECONFIG", ""),
		NodeID:           getEnv("NODE_ID", "k8s-executor"),
	}
}

// ── Metrics ───────────────────────────────────────────────────────────────────

var (
	podsCreated = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_k8s_pods_created_total", Help: "Total pods created",
	})
	podsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "platform_k8s_pods_failed_total", Help: "Total pod failures",
	})
	podsRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "platform_k8s_pods_running", Help: "Current running pods",
	})
	podStartDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "platform_k8s_pod_start_seconds",
		Help:    "Time from pod creation to Running phase",
		Buckets: []float64{1, 2, 5, 10, 30, 60, 120},
	})
)

func init() {
	prometheus.MustRegister(podsCreated, podsFailed, podsRunning, podStartDuration)
}

// ── Request / Response types ─────────────────────────────────────────────────

type StartRequest struct {
	AgentID     string            `json:"agent_id" binding:"required"`
	Image       string            `json:"image" binding:"required"`
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	CPULimit    string            `json:"cpu_limit"`    // e.g. "500m"
	MemoryLimit string            `json:"memory_limit"` // e.g. "512Mi"
	CPURequest  string            `json:"cpu_request"`
	MemRequest  string            `json:"mem_request"`
	EnvVars     map[string]string `json:"env_vars"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	NodeSelector map[string]string `json:"node_selector"`
	Command     []string          `json:"command"`
	Args        []string          `json:"args"`
	// Job mode: run once and complete
	JobMode     bool              `json:"job_mode"`
	// Restart policy: Never | OnFailure | Always
	RestartPolicy string         `json:"restart_policy"`
	ServiceAccount string        `json:"service_account"`
	PullPolicy  string            `json:"pull_policy"` // Always|IfNotPresent|Never
}

type StopRequest struct {
	AgentID   string `json:"agent_id" binding:"required"`
	PodName   string `json:"pod_name"`
	Namespace string `json:"namespace"`
}

// ── Executor ─────────────────────────────────────────────────────────────────

type K8sExecutor struct {
	cfg    Config
	kc     *kubernetes.Clientset
	nc     *natsgo.Conn
	http   *http.Client
	logger *slog.Logger
	// track agent → pod mapping for log streaming cleanup
	streams sync.Map // agentID → cancel func
}

func NewK8sExecutor(cfg Config, kc *kubernetes.Clientset, nc *natsgo.Conn) *K8sExecutor {
	return &K8sExecutor{
		cfg:    cfg,
		kc:     kc,
		nc:     nc,
		http:   &http.Client{Timeout: 30 * time.Second},
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ── Pod creation ──────────────────────────────────────────────────────────────

func (e *K8sExecutor) StartPod(ctx context.Context, req StartRequest) (string, error) {
	ns := orDefault(req.Namespace, e.cfg.DefaultNamespace)
	podName := e.podName(req.AgentID, req.Name)

	// Ensure namespace exists
	if err := e.ensureNamespace(ctx, ns); err != nil {
		return "", fmt.Errorf("namespace: %w", err)
	}

	// Build resource requirements
	resReq := corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{},
		Requests: corev1.ResourceList{},
	}
	if req.CPULimit != "" {
		resReq.Limits[corev1.ResourceCPU] = resource.MustParse(req.CPULimit)
	}
	if req.MemoryLimit != "" {
		resReq.Limits[corev1.ResourceMemory] = resource.MustParse(req.MemoryLimit)
	}
	if req.CPURequest != "" {
		resReq.Requests[corev1.ResourceCPU] = resource.MustParse(req.CPURequest)
	} else if req.CPULimit != "" {
		resReq.Requests[corev1.ResourceCPU] = resource.MustParse(req.CPULimit)
	}
	if req.MemRequest != "" {
		resReq.Requests[corev1.ResourceMemory] = resource.MustParse(req.MemRequest)
	} else if req.MemoryLimit != "" {
		resReq.Requests[corev1.ResourceMemory] = resource.MustParse(req.MemoryLimit)
	}

	// Build environment
	envVars := []corev1.EnvVar{
		{Name: "AGENT_ID", Value: req.AgentID},
		{Name: "NATS_URL", Value: e.cfg.NatsURL},
		{Name: "CONTROL_PLANE_URL", Value: e.cfg.ControlPlaneURL},
	}
	for k, v := range req.EnvVars {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	// Labels
	labels := map[string]string{
		"app":                     "ai-agent",
		"platform.managed":        "true",
		"platform.agent-id":       req.AgentID,
		"platform.executor-node":  e.cfg.NodeID,
	}
	for k, v := range req.Labels {
		labels[k] = v
	}

	// Annotations
	annotations := map[string]string{
		"platform.io/agent-id":    req.AgentID,
		"platform.io/executor":    e.cfg.NodeID,
		"platform.io/created-at":  time.Now().UTC().Format(time.RFC3339),
	}
	for k, v := range req.Annotations {
		annotations[k] = v
	}

	pullPolicy := corev1.PullIfNotPresent
	if req.PullPolicy == "Always" {
		pullPolicy = corev1.PullAlways
	} else if req.PullPolicy == "Never" {
		pullPolicy = corev1.PullNever
	}

	restartPolicy := corev1.RestartPolicyNever
	if !req.JobMode {
		switch req.RestartPolicy {
		case "Always":
			restartPolicy = corev1.RestartPolicyAlways
		case "OnFailure":
			restartPolicy = corev1.RestartPolicyOnFailure
		default:
			restartPolicy = corev1.RestartPolicyNever
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   ns,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:      restartPolicy,
			NodeSelector:       req.NodeSelector,
			ServiceAccountName: orDefault(req.ServiceAccount, "default"),
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           req.Image,
					Command:         req.Command,
					Args:            req.Args,
					Env:             envVars,
					Resources:       resReq,
					ImagePullPolicy: pullPolicy,
					// Security: drop all capabilities, read-only root FS
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolPtr(false),
						ReadOnlyRootFilesystem:   boolPtr(true),
						RunAsNonRoot:             boolPtr(true),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
				},
			},
			// EmptyDir for writable scratch space when rootfs is read-only
			Volumes: []corev1.Volume{
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			TerminationGracePeriodSeconds: int64Ptr(30),
		},
	}

	// Mount tmp volume
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "tmp", MountPath: "/tmp"},
	}

	start := time.Now()
	created, err := e.kc.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		podsFailed.Inc()
		return "", fmt.Errorf("create pod: %w", err)
	}

	podsCreated.Inc()
	e.logger.Info("pod created", "pod", created.Name, "namespace", ns, "agent_id", req.AgentID)

	// Start async log streamer and pod watcher
	streamCtx, cancelStream := context.WithCancel(context.Background())
	e.streams.Store(req.AgentID, cancelStream)
	go e.streamLogs(streamCtx, req.AgentID, ns, podName, start)
	go e.watchPod(req.AgentID, ns, podName)

	return podName, nil
}

// ── Log streaming ─────────────────────────────────────────────────────────────

func (e *K8sExecutor) streamLogs(ctx context.Context, agentID, ns, podName string, podStarted time.Time) {
	// Wait for pod to be Running
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pod, err := e.kc.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if pod.Status.Phase == corev1.PodRunning ||
				pod.Status.Phase == corev1.PodSucceeded ||
				pod.Status.Phase == corev1.PodFailed {
				podStartDuration.Observe(time.Since(podStarted).Seconds())
				goto streaming
			}
		}
	}

streaming:
	opts := &corev1.PodLogOptions{
		Container: "agent",
		Follow:    true,
		Previous:  false,
		Timestamps: true,
	}

	stream, err := e.kc.CoreV1().Pods(ns).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		e.logger.Warn("failed to open log stream", "pod", podName, "error", err)
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if ctx.Err() != nil {
			return
		}

		level := detectLevel(line)
		payload, _ := json.Marshal(map[string]interface{}{
			"agent_id":   agentID,
			"message":    line,
			"level":      level,
			"source":     "k8s-pod",
			"pod_name":   podName,
			"namespace":  ns,
			"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
		})
		_ = e.nc.Publish("logs.stream", payload)
		_ = e.nc.Publish("agent.logs."+agentID, payload)

		// Also persist via control plane
		go e.persistLog(agentID, line, level)
	}
}

func (e *K8sExecutor) persistLog(agentID, message, level string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"agent_id": agentID, "message": message, "level": level,
		"source": "k8s-executor", "timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	req, err := http.NewRequestWithContext(context.Background(), "POST",
		e.cfg.ControlPlaneURL+"/api/v1/logs", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.ControlPlaneAPIKey != "" {
		req.Header.Set("X-Service-API-Key", e.cfg.ControlPlaneAPIKey)
	}
	resp, err := e.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// ── Pod watcher ───────────────────────────────────────────────────────────────

func (e *K8sExecutor) watchPod(agentID, ns, podName string) {
	ctx := context.Background()
	watcher, err := e.kc.CoreV1().Pods(ns).Watch(ctx, metav1.ListOptions{
		FieldSelector:  "metadata.name=" + podName,
		TimeoutSeconds: int64Ptr(int64(e.cfg.PodTTLSeconds)),
	})
	if err != nil {
		e.logger.Error("failed to watch pod", "pod", podName, "error", err)
		return
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}

		phase := string(pod.Status.Phase)
		var natsSubject string
		var agentStatus string

		switch event.Type {
		case watch.Modified:
			switch pod.Status.Phase {
			case corev1.PodRunning:
				podsRunning.Inc()
				natsSubject = "agents.events"
				agentStatus = "running"
				e.notifyControlPlane(agentID, "running", podName, "")

			case corev1.PodSucceeded:
				podsRunning.Dec()
				natsSubject = "agents.events"
				agentStatus = "stopped"
				e.notifyControlPlane(agentID, "stopped", podName, "")
				e.cleanup(agentID)
				return

			case corev1.PodFailed:
				podsFailed.Inc()
				podsRunning.Dec()
				reason := podFailReason(pod)
				natsSubject = "agents.events"
				agentStatus = "failed"
				e.notifyControlPlane(agentID, "failed", podName, reason)
				e.cleanup(agentID)
				return
			}

		case watch.Deleted:
			podsRunning.Dec()
			e.cleanup(agentID)
			return
		}

		if natsSubject != "" {
			payload, _ := json.Marshal(map[string]interface{}{
				"event_type": "agent." + agentStatus,
				"agent_id":   agentID,
				"pod_name":   podName,
				"namespace":  ns,
				"phase":      phase,
				"source":     "k8s-executor",
				"timestamp":  time.Now().UTC().Format(time.RFC3339),
			})
			_ = e.nc.Publish(natsSubject, payload)
		}
	}
}

func (e *K8sExecutor) notifyControlPlane(agentID, status, podName, errMsg string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"status": status, "pod_name": podName, "error": errMsg,
		"runtime": "kubernetes",
	})
	req, err := http.NewRequestWithContext(context.Background(), "PATCH",
		e.cfg.ControlPlaneURL+"/api/v1/agents/"+agentID+"/runtime-status",
		bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.ControlPlaneAPIKey != "" {
		req.Header.Set("X-Service-API-Key", e.cfg.ControlPlaneAPIKey)
	}
	resp, err := e.http.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (e *K8sExecutor) cleanup(agentID string) {
	if cancel, ok := e.streams.LoadAndDelete(agentID); ok {
		cancel.(context.CancelFunc)()
	}
}

// ── Stop pod ──────────────────────────────────────────────────────────────────

func (e *K8sExecutor) StopPod(ctx context.Context, req StopRequest) error {
	ns := orDefault(req.Namespace, e.cfg.DefaultNamespace)
	podName := req.PodName
	if podName == "" {
		podName = e.podName(req.AgentID, "")
	}

	gracePeriod := int64(30)
	err := e.kc.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil {
		return fmt.Errorf("delete pod %s: %w", podName, err)
	}

	e.cleanup(req.AgentID)
	e.logger.Info("pod deleted", "pod", podName, "namespace", ns, "agent_id", req.AgentID)
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (e *K8sExecutor) podName(agentID, name string) string {
	id := agentID
	if len(id) > 8 {
		id = id[:8]
	}
	if name != "" {
		// sanitize: lowercase, replace non-alphanumeric
		safe := strings.ToLower(name)
		safe = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				return r
			}
			return '-'
		}, safe)
		if len(safe) > 40 {
			safe = safe[:40]
		}
		return fmt.Sprintf("agent-%s-%s", safe, id)
	}
	return fmt.Sprintf("agent-%s", id)
}

func (e *K8sExecutor) ensureNamespace(ctx context.Context, ns string) error {
	_, err := e.kc.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	return err
}

func detectLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "fatal"):
		return "error"
	case strings.Contains(lower, "warn"):
		return "warn"
	case strings.Contains(lower, "debug"):
		return "debug"
	default:
		return "info"
	}
}

func podFailReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return fmt.Sprintf("exit_code=%d reason=%s", cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	return pod.Status.Message
}

// ── HTTP API ──────────────────────────────────────────────────────────────────

func (e *K8sExecutor) setupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "healthy", "service": "k8s-executor", "node_id": e.cfg.NodeID})
	})
	r.GET("/ready", func(c *gin.Context) {
		_, err := e.kc.CoreV1().Namespaces().List(c.Request.Context(), metav1.ListOptions{Limit: 1})
		if err != nil {
			c.JSON(503, gin.H{"status": "not_ready", "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	requireServiceAuth := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			got := c.GetHeader("X-Service-API-Key")
			if e.cfg.ControlPlaneAPIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(e.cfg.ControlPlaneAPIKey)) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}
	protected := r.Group("/")
	protected.Use(requireServiceAuth())

	// Start a pod
	protected.POST("/pods/start", func(c *gin.Context) {
		var req StartRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		podName, err := e.StartPod(c.Request.Context(), req)
		if err != nil {
			e.logger.Error("start pod failed", "error", err, "agent_id", req.AgentID)
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"success": true, "pod_name": podName,
			"namespace": orDefault(req.Namespace, e.cfg.DefaultNamespace),
			"agent_id": req.AgentID, "runtime": "kubernetes",
		})
	})

	// Stop / delete a pod
	protected.POST("/pods/stop", func(c *gin.Context) {
		var req StopRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := e.StopPod(c.Request.Context(), req); err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"success": true, "agent_id": req.AgentID})
	})

	// List pods managed by this executor
	protected.GET("/pods", func(c *gin.Context) {
		ns := c.DefaultQuery("namespace", e.cfg.DefaultNamespace)
		pods, err := e.kc.CoreV1().Pods(ns).List(c.Request.Context(), metav1.ListOptions{
			LabelSelector: "platform.managed=true,platform.executor-node=" + e.cfg.NodeID,
		})
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		result := make([]map[string]interface{}, 0, len(pods.Items))
		for _, pod := range pods.Items {
			result = append(result, map[string]interface{}{
				"name":      pod.Name,
				"namespace": pod.Namespace,
				"phase":     pod.Status.Phase,
				"agent_id":  pod.Labels["platform.agent-id"],
				"node":      pod.Spec.NodeName,
				"started":   pod.Status.StartTime,
			})
		}
		c.JSON(200, result)
	})

	// Get pod status
	protected.GET("/pods/:namespace/:pod_name", func(c *gin.Context) {
		pod, err := e.kc.CoreV1().Pods(c.Param("namespace")).
			Get(c.Request.Context(), c.Param("pod_name"), metav1.GetOptions{})
		if err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{
			"name": pod.Name, "namespace": pod.Namespace,
			"phase":      pod.Status.Phase,
			"conditions": pod.Status.Conditions,
			"container_statuses": pod.Status.ContainerStatuses,
			"node_name":  pod.Spec.NodeName,
		})
	})

	return r
}

// ── Heartbeat ─────────────────────────────────────────────────────────────────

func (e *K8sExecutor) startHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	hc := &http.Client{Timeout: 5 * time.Second}
	send := func() {
		payload, _ := json.Marshal(map[string]interface{}{
			"node_id":  e.cfg.NodeID,
			"hostname": e.cfg.NodeID,
			"address":  "http://k8s-executor:" + e.cfg.Port,
			"port":     8091,
		})
		req, err := http.NewRequestWithContext(ctx, "POST",
			e.cfg.ControlPlaneURL+"/api/v1/nodes/heartbeat", bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if e.cfg.ControlPlaneAPIKey != "" {
			req.Header.Set("X-Service-API-Key", e.cfg.ControlPlaneAPIKey)
		}
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
	send()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()
	logger.Info("starting k8s-executor", "port", cfg.Port, "namespace", cfg.DefaultNamespace)

	// ── Kubernetes client ──────────────────────────────────────────────
	var k8sCfg *rest.Config
	var err error
	if cfg.KubeconfigPath != "" {
		k8sCfg, err = clientcmd.BuildConfigFromFlags("", cfg.KubeconfigPath)
	} else {
		k8sCfg, err = rest.InClusterConfig()
	}
	if err != nil {
		logger.Error("failed to build k8s config", "error", err)
		os.Exit(1)
	}

	kc, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		logger.Error("failed to create k8s client", "error", err)
		os.Exit(1)
	}

	// Sanity check
	version, err := kc.Discovery().ServerVersion()
	if err != nil {
		logger.Error("cannot reach kubernetes API", "error", err)
		os.Exit(1)
	}
	logger.Info("kubernetes connected", "version", version.GitVersion)

	// ── NATS ───────────────────────────────────────────────────────────
	var nc *natsgo.Conn
	for i := 0; i < 30; i++ {
		nc, err = natsgo.Connect(cfg.NatsURL,
			natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second),
			natsgo.Name("k8s-executor"))
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

	executor := NewK8sExecutor(cfg, kc, nc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go executor.startHeartbeat(ctx)

	router := executor.setupRouter()
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: router}

	go func() {
		logger.Info("k8s-executor listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http error", "error", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	logger.Info("shutdown signal received")
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	logger.Info("k8s-executor stopped")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func orDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
func boolPtr(b bool) *bool         { return &b }
func int64Ptr(i int64) *int64      { return &i }

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
