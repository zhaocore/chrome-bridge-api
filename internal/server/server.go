package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"agent-webbridge-api/internal/bridge"
	"agent-webbridge-api/internal/files"
	"agent-webbridge-api/internal/session"

	"github.com/gin-gonic/gin"
)

const DefaultPort = 10086

type Config struct {
	Version string
	Host    string
	Port    int
}

type Server struct {
	cfg      Config
	started  time.Time
	bridge   *bridge.Manager
	sessions *session.Store
	router   *gin.Engine
}

type CommandRequest struct {
	Action    string         `json:"action"`
	Args      map[string]any `json:"args"`
	Session   string         `json:"session"`
	TimeoutMS int            `json:"timeout_ms"`
}

func New(cfg Config, bridgeManager *bridge.Manager, sessions *session.Store) *Server {
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if bridgeManager == nil {
		bridgeManager = bridge.NewManager(cfg.Version, nil)
	}
	if sessions == nil {
		sessions = session.NewStore()
	}
	gin.SetMode(gin.ReleaseMode)
	s := &Server{
		cfg:      cfg,
		started:  time.Now(),
		bridge:   bridgeManager,
		sessions: sessions,
	}
	s.router = s.routes()
	return s
}

func (s *Server) Router() http.Handler {
	return s.router
}

func (s *Server) Bridge() *bridge.Manager {
	return s.bridge
}

func (s *Server) routes() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/status", s.handleStatus)
	r.POST("/command", s.handleCommand)
	r.GET("/tools", s.handleTools)
	r.POST("/api/connections", s.handleConnection)
	r.GET("/ws", func(c *gin.Context) {
		s.bridge.ServeWS(c.Writer, c.Request)
	})
	return r
}

func (s *Server) handleStatus(c *gin.Context) {
	status := s.bridge.Status()
	c.JSON(http.StatusOK, gin.H{
		"running":             true,
		"port":                s.cfg.Port,
		"version":             s.cfg.Version,
		"extension_connected": status.Connected,
		"extension_id":        status.ExtensionID,
		"extension_version":   status.ExtensionVersion,
		"uptime_seconds":      int(time.Since(s.started).Seconds()),
	})
}

func (s *Server) handleTools(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"tools": toolMetas})
}

func (s *Server) handleConnection(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"url":  "ws://" + s.cfg.Host + ":" + strconv.Itoa(s.cfg.Port) + "/ws",
		"port": s.cfg.Port,
	})
}

func (s *Server) handleCommand(c *gin.Context) {
	var req CommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Args == nil {
		req.Args = map[string]any{}
	}
	if err := ValidateTool(req.Action, req.Args); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	timeout := 30 * time.Second
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()

	args := s.sessions.Prepare(req.Action, req.Args, req.Session)
	data, err := s.bridge.Call(ctx, req.Action, args)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		if errors.Is(err, bridge.ErrExtensionNotConnected) {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}

	data, err = files.Normalize(req.Action, req.Args, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	s.sessions.Update(req.Action, req.Session, data)
	c.JSON(http.StatusOK, gin.H{"data": data})
}

func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.cfg.Host+":"+strconv.Itoa(s.cfg.Port), s.router)
}
