package tcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"

	"drip/internal/server/tunnel"
	"drip/internal/shared/constants"
	"drip/internal/shared/httputil"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

type ConnectionConfig struct {
	Conn         net.Conn
	AuthToken    string
	Manager      *tunnel.Manager
	Logger       *zap.Logger
	PortAlloc    *PortAllocator
	Domain       string
	TunnelDomain string
	PublicPort   int
	HTTPHandler  http.Handler
	GroupManager *ConnectionGroupManager
	HTTPListener *connQueueListener
}

type Connection struct {
	conn             net.Conn
	authToken        string
	manager          *tunnel.Manager
	logger           *zap.Logger
	subdomain        string
	port             int
	domain           string
	tunnelDomain     string
	publicPort       int
	portAlloc        *PortAllocator
	tunnelConn       *tunnel.Connection
	stopCh           chan struct{}
	once             sync.Once
	lastHeartbeat    time.Time
	mu               sync.RWMutex
	frameWriter      *protocol.FrameWriter
	httpHandler      http.Handler
	tunnelType       protocol.TunnelType
	ctx              context.Context
	cancel           context.CancelFunc
	session          *yamux.Session
	proxy            *Proxy
	tunnelID         string
	groupManager     *ConnectionGroupManager
	httpListener     *connQueueListener
	handedOff        bool
	lifecycleManager *ConnectionLifecycleManager

	// Server capabilities
	allowedTunnelTypes []string
	allowedTransports  []string
}

// NewConnection creates a new connection handler
func NewConnection(cfg ConnectionConfig) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	stopCh := make(chan struct{})

	c := &Connection{
		conn:             cfg.Conn,
		authToken:        cfg.AuthToken,
		manager:          cfg.Manager,
		logger:           cfg.Logger,
		portAlloc:        cfg.PortAlloc,
		domain:           cfg.Domain,
		tunnelDomain:     cfg.TunnelDomain,
		publicPort:       cfg.PublicPort,
		httpHandler:      cfg.HTTPHandler,
		stopCh:           stopCh,
		lastHeartbeat:    time.Now(),
		ctx:              ctx,
		cancel:           cancel,
		groupManager:     cfg.GroupManager,
		httpListener:     cfg.HTTPListener,
		lifecycleManager: NewConnectionLifecycleManager(stopCh, cancel, cfg.Logger),
	}

	// Set connection in lifecycle manager
	c.lifecycleManager.SetConnection(cfg.Conn)

	return c
}

func (c *Connection) Handle() error {
	protocol.RegisterConnection()
	defer c.Close()

	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(c.conn)

	peek, err := reader.Peek(4)
	if err != nil {
		return fmt.Errorf("failed to peek connection: %w", err)
	}

	if httputil.IsHTTPRequest(peek) {
		c.logger.Info("Detected HTTP request on TCP port, handling as HTTP")
		return c.handleHTTPRequest(reader)
	}

	// Check if TCP transport is allowed (only for Drip protocol connections, not HTTP)
	if !c.isTransportAllowed("tcp") {
		c.logger.Warn("TCP transport not allowed, rejecting Drip protocol connection")
		return fmt.Errorf("TCP transport not allowed")
	}

	frame, err := protocol.ReadFrame(reader)
	if err != nil {
		return fmt.Errorf("failed to read registration frame: %w", err)
	}
	sf := protocol.WithFrame(frame)
	defer sf.Close()

	if sf.Frame.Type == protocol.FrameTypeDataConnect {
		handler := NewDataConnectionHandler(
			c.conn,
			reader,
			c.authToken,
			c.groupManager,
			c.stopCh,
			c.logger,
		)
		handler.SetSessionCreatedHandler(func(session *yamux.Session) {
			c.session = session
			if c.lifecycleManager != nil {
				c.lifecycleManager.SetSession(session)
			}
		})
		handler.SetTunnelIDHandler(func(tunnelID string) {
			c.tunnelID = tunnelID
		})
		return handler.Handle(sf.Frame)
	}

	if sf.Frame.Type != protocol.FrameTypeRegister {
		return fmt.Errorf("expected register frame, got %s", sf.Frame.Type)
	}

	var req protocol.RegisterRequest
	if err := json.Unmarshal(sf.Frame.Payload, &req); err != nil {
		return fmt.Errorf("failed to parse registration request: %w", err)
	}

	c.tunnelType = req.TunnelType

	// Check if tunnel type is allowed
	if !c.isTunnelTypeAllowed(string(req.TunnelType)) {
		c.sendError("tunnel_type_not_allowed", fmt.Sprintf("Tunnel type '%s' is not allowed on this server", req.TunnelType))
		return fmt.Errorf("tunnel type not allowed: %s", req.TunnelType)
	}

	if c.authToken != "" && req.Token != c.authToken {
		c.sendError("authentication_failed", "Invalid authentication token")
		return fmt.Errorf("authentication failed")
	}

	// Use RegistrationHandler for registration logic
	regHandler := NewRegistrationHandler(
		c.manager,
		c.portAlloc,
		c.groupManager,
		c.domain,
		c.tunnelDomain,
		c.publicPort,
		c.logger,
	)

	regReq := &RegistrationRequest{
		TunnelType:       req.TunnelType,
		CustomSubdomain:  req.CustomSubdomain,
		CustomHost:       req.CustomHost,
		Token:            req.Token,
		ConnectionType:   req.ConnectionType,
		PoolCapabilities: req.PoolCapabilities,
		IPAccess:         req.IPAccess,
		ProxyAuth:        req.ProxyAuth,
		LocalPort:        req.LocalPort,
	}

	result, err := regHandler.Register(regReq)
	if err != nil {
		c.sendError("registration_failed", err.Error())
		return fmt.Errorf("registration failed: %w", err)
	}

	// Store registration results
	c.subdomain = result.Subdomain
	c.port = result.Port
	c.tunnelConn = result.TunnelConn
	c.tunnelConn.Conn = nil

	// Update lifecycle manager with registration info
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetPortAllocation(c.portAlloc, c.port)
		c.lifecycleManager.SetTunnelRegistration(c.manager, c.subdomain, "", c.groupManager)
	}

	// Handle connection groups
	if result.SupportsDataConn && c.groupManager != nil {
		group := c.groupManager.CreateGroup(result.Subdomain, req.Token, c, req.TunnelType)
		result.TunnelID = group.TunnelID
		c.tunnelID = result.TunnelID

		// Update lifecycle manager with tunnel ID
		if c.lifecycleManager != nil {
			c.lifecycleManager.SetTunnelRegistration(c.manager, c.subdomain, c.tunnelID, c.groupManager)
		}

		c.logger.Info("Created connection group for multi-connection support",
			zap.String("tunnel_id", result.TunnelID),
			zap.Int("max_data_conns", req.PoolCapabilities.MaxDataConns),
		)
	}

	// Build and send registration response
	resp, err := regHandler.BuildRegistrationResponse(result)
	if err != nil {
		return fmt.Errorf("failed to build registration response: %w", err)
	}

	if err := regHandler.SendRegistrationResponse(c.conn, resp); err != nil {
		return fmt.Errorf("failed to send registration ack: %w", err)
	}

	c.conn.SetReadDeadline(time.Time{})

	if req.TunnelType == protocol.TunnelTypeTCP {
		return c.handleTCPTunnel(reader)
	}
	if req.TunnelType == protocol.TunnelTypeHTTP || req.TunnelType == protocol.TunnelTypeHTTPS {
		return c.handleHTTPProxyTunnel(reader)
	}

	c.frameWriter = protocol.NewFrameWriter(c.conn)

	// Update lifecycle manager with frame writer
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetFrameWriter(c.frameWriter)
	}

	c.frameWriter.SetWriteErrorHandler(func(err error) {
		c.logger.Error("Write error detected, closing connection", zap.Error(err))
		c.Close()
	})

	go c.heartbeatChecker()

	// Use FrameHandler for frame processing
	frameHandler := NewFrameHandler(c.conn, reader, c.stopCh, c.frameWriter, c.logger)
	frameHandler.SetHeartbeatHandler(func() {
		c.handleHeartbeat()
	})
	frameHandler.SetCloseHandler(func() {
		c.logger.Info("Client requested close")
	})

	return frameHandler.HandleFrames()
}

func (c *Connection) handleHTTPRequest(reader *bufio.Reader) error {
	handler := NewHTTPRequestHandler(
		c.conn,
		reader,
		c.httpHandler,
		c.httpListener,
		c.ctx,
		c.logger,
		&c.mu,
		&c.handedOff,
	)
	return handler.Handle()
}

func (c *Connection) IsHandedOff() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.handedOff
}

func parseTCPSubdomainPort(subdomain string) (int, bool) {
	if !strings.HasPrefix(subdomain, "tcp-") {
		return 0, false
	}

	portStr := strings.TrimPrefix(subdomain, "tcp-")
	if portStr == "" {
		return 0, false
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}

	return port, true
}

func (c *Connection) handleHeartbeat() {
	c.mu.Lock()
	c.lastHeartbeat = time.Now()
	c.mu.Unlock()

	ackFrame := protocol.NewFrame(protocol.FrameTypeHeartbeatAck, nil)
	err := c.frameWriter.WriteControl(ackFrame)
	if err != nil {
		c.logger.Error("Failed to send heartbeat ack", zap.Error(err))
	}
}

func (c *Connection) heartbeatChecker() {
	ticker := time.NewTicker(constants.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.RLock()
			lastHB := c.lastHeartbeat
			c.mu.RUnlock()

			if time.Since(lastHB) > constants.HeartbeatTimeout {
				c.logger.Warn("Heartbeat timeout",
					zap.String("subdomain", c.subdomain),
					zap.Duration("last_heartbeat", time.Since(lastHB)),
				)
				c.Close()
				return
			}
		}
	}
}

func (c *Connection) sendError(code, message string) {
	errMsg := protocol.ErrorMessage{
		Code:    code,
		Message: message,
	}
	data, err := json.Marshal(errMsg)
	if err != nil {
		c.logger.Error("Failed to marshal error message", zap.Error(err))
		return
	}
	errFrame := protocol.NewFrame(protocol.FrameTypeError, data)

	if c.frameWriter == nil {
		_ = protocol.WriteFrame(c.conn, errFrame)
	} else {
		c.frameWriter.WriteFrame(errFrame)
	}
}

func (c *Connection) Close() {
	c.once.Do(func() {
		// Check if connection was handed off to HTTP handler
		c.mu.RLock()
		handedOff := c.handedOff
		c.mu.RUnlock()

		// If handed off, don't close the connection - HTTP handler owns it now
		if handedOff {
			c.logger.Debug("Connection handed off to HTTP handler, skipping close")
			return
		}

		// Use lifecycle manager for cleanup
		if c.lifecycleManager != nil {
			c.lifecycleManager.Close()
		} else {
			// Fallback if lifecycle manager not initialized
			protocol.UnregisterConnection()
			close(c.stopCh)

			if c.cancel != nil {
				c.cancel()
			}

			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn != nil {
				_ = conn.SetDeadline(time.Now())
			}

			if c.frameWriter != nil {
				c.frameWriter.Close()
			}

			if c.proxy != nil {
				c.proxy.Stop()
			}

			if c.session != nil {
				_ = c.session.Close()
			}

			if conn != nil {
				conn.Close()
			}

			if c.port > 0 && c.portAlloc != nil {
				c.portAlloc.Release(c.port)
			}

			if c.subdomain != "" {
				c.manager.Unregister(c.subdomain)
				if c.tunnelID != "" && c.groupManager != nil {
					c.groupManager.RemoveGroup(c.tunnelID)
				}
			}

			c.logger.Info("Connection closed",
				zap.String("subdomain", c.subdomain),
			)
		}
	})
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "i/o timeout")
}

// SetAllowedTunnelTypes sets the allowed tunnel types for this connection
func (c *Connection) SetAllowedTunnelTypes(types []string) {
	c.allowedTunnelTypes = types
}

// SetAllowedTransports sets the allowed transports for this connection
func (c *Connection) SetAllowedTransports(transports []string) {
	c.allowedTransports = transports
}

// isTransportAllowed checks if a transport is allowed
func (c *Connection) isTransportAllowed(transport string) bool {
	if len(c.allowedTransports) == 0 {
		return true
	}
	for _, t := range c.allowedTransports {
		if strings.EqualFold(t, transport) {
			return true
		}
	}
	return false
}

// isTunnelTypeAllowed checks if a tunnel type is allowed
func (c *Connection) isTunnelTypeAllowed(tunnelType string) bool {
	if len(c.allowedTunnelTypes) == 0 {
		return true // Allow all by default
	}
	for _, t := range c.allowedTunnelTypes {
		if strings.EqualFold(t, tunnelType) {
			return true
		}
	}
	return false
}
