package tcp

import (
	"fmt"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
	"drip/internal/shared/utils"
)

// RegistrationHandler handles tunnel registration logic.
type RegistrationHandler struct {
	manager      *tunnel.Manager
	portAlloc    *PortAllocator
	groupManager *ConnectionGroupManager
	domain       string
	tunnelDomain string
	publicPort   int
	logger       *zap.Logger
}

// NewRegistrationHandler creates a new registration handler.
func NewRegistrationHandler(
	manager *tunnel.Manager,
	portAlloc *PortAllocator,
	groupManager *ConnectionGroupManager,
	domain, tunnelDomain string,
	publicPort int,
	logger *zap.Logger,
) *RegistrationHandler {
	return &RegistrationHandler{
		manager:      manager,
		portAlloc:    portAlloc,
		groupManager: groupManager,
		domain:       domain,
		tunnelDomain: tunnelDomain,
		publicPort:   publicPort,
		logger:       logger,
	}
}

// RegistrationRequest contains all information needed for registration.
type RegistrationRequest struct {
	TunnelType       protocol.TunnelType
	CustomSubdomain  string
	CustomHost       string // Custom domain (CNAME) for this tunnel
	Token            string
	ConnectionType   string
	PoolCapabilities *protocol.PoolCapabilities
	IPAccess         *protocol.IPAccessControl
	ProxyAuth        *protocol.ProxyAuth
	LocalPort        int
}

// RegistrationResult contains the result of a registration attempt.
type RegistrationResult struct {
	Subdomain        string
	Port             int
	TunnelURL        string
	TunnelID         string
	SupportsDataConn bool
	RecommendedConns int
	TunnelConn       *tunnel.Connection
}

// Register handles the tunnel registration process.
func (rh *RegistrationHandler) Register(req *RegistrationRequest) (*RegistrationResult, error) {
	// Allocate port for TCP tunnels
	port := 0
	if req.TunnelType == protocol.TunnelTypeTCP {
		if rh.portAlloc == nil {
			return nil, fmt.Errorf("port allocator not configured")
		}

		if requestedPort, ok := parseTCPSubdomainPort(req.CustomSubdomain); ok {
			allocatedPort, err := rh.portAlloc.AllocateSpecific(requestedPort)
			if err != nil {
				return nil, fmt.Errorf("failed to allocate requested port %d: %w", requestedPort, err)
			}
			port = allocatedPort
		} else {
			allocatedPort, err := rh.portAlloc.Allocate()
			if err != nil {
				return nil, fmt.Errorf("failed to allocate port: %w", err)
			}
			port = allocatedPort

			if req.CustomSubdomain == "" {
				req.CustomSubdomain = fmt.Sprintf("tcp-%d", port)
			}
		}
	}

	// Register with tunnel manager
	subdomain, err := rh.manager.Register(nil, req.CustomSubdomain)
	if err != nil {
		if port > 0 && rh.portAlloc != nil {
			rh.portAlloc.Release(port)
		}
		return nil, fmt.Errorf("tunnel registration failed: %w", err)
	}

	// Get tunnel connection
	tunnelConn, ok := rh.manager.Get(subdomain)
	if !ok {
		return nil, fmt.Errorf("failed to get registered tunnel")
	}

	// Configure tunnel
	tunnelConn.SetTunnelType(req.TunnelType)

	if req.IPAccess != nil && (len(req.IPAccess.AllowIPs) > 0 || len(req.IPAccess.DenyIPs) > 0) {
		tunnelConn.SetIPAccessControl(req.IPAccess.AllowIPs, req.IPAccess.DenyIPs)
		rh.logger.Info("IP access control configured",
			zap.String("subdomain", subdomain),
			zap.Strings("allow_ips", req.IPAccess.AllowIPs),
			zap.Strings("deny_ips", req.IPAccess.DenyIPs),
		)
	}

	if req.ProxyAuth != nil && req.ProxyAuth.Enabled {
		tunnelConn.SetProxyAuth(req.ProxyAuth)
		rh.logger.Info("Proxy authentication configured",
			zap.String("subdomain", subdomain),
		)
	}

	// Set custom host if provided
	if req.CustomHost != "" {
		tunnelConn.SetCustomHost(req.CustomHost)
		rh.logger.Info("Custom host configured",
			zap.String("subdomain", subdomain),
			zap.String("custom_host", req.CustomHost),
		)
	}

	// Build tunnel URL
	urlBuilder := utils.NewTunnelURLBuilder(rh.tunnelDomain, rh.publicPort)
	tunnelURL := urlBuilder.BuildURL(subdomain, req.TunnelType, port)

	// Override with custom host URL if provided
	if req.CustomHost != "" {
		scheme := "https"
		if req.TunnelType == protocol.TunnelTypeTCP {
			tunnelURL = fmt.Sprintf("tcp://%s:%d", req.CustomHost, port)
		} else {
			port := rh.publicPort
			if port == 0 || port == 443 {
				tunnelURL = fmt.Sprintf("%s://%s", scheme, req.CustomHost)
			} else {
				tunnelURL = fmt.Sprintf("%s://%s:%d", scheme, req.CustomHost, port)
			}
		}
	}

	// Handle connection groups for multi-connection support
	var tunnelID string
	var supportsDataConn bool
	recommendedConns := 0

	if req.PoolCapabilities != nil && req.ConnectionType == "primary" && rh.groupManager != nil {
		// This will be handled by the caller since it needs the connection instance
		supportsDataConn = true
		recommendedConns = 4
	}

	rh.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("tunnel_type", string(req.TunnelType)),
		zap.Int("local_port", req.LocalPort),
		zap.Int("remote_port", port),
	)

	return &RegistrationResult{
		Subdomain:        subdomain,
		Port:             port,
		TunnelURL:        tunnelURL,
		TunnelID:         tunnelID,
		SupportsDataConn: supportsDataConn,
		RecommendedConns: recommendedConns,
		TunnelConn:       tunnelConn,
	}, nil
}

// BuildRegistrationResponse creates a protocol registration response.
func (rh *RegistrationHandler) BuildRegistrationResponse(result *RegistrationResult) (*protocol.RegisterResponse, error) {
	resp := &protocol.RegisterResponse{
		Subdomain:        result.Subdomain,
		Port:             result.Port,
		URL:              result.TunnelURL,
		Message:          "Tunnel registered successfully",
		TunnelID:         result.TunnelID,
		SupportsDataConn: result.SupportsDataConn,
		RecommendedConns: result.RecommendedConns,
	}
	return resp, nil
}

// SendRegistrationResponse sends the registration response frame.
func (rh *RegistrationHandler) SendRegistrationResponse(conn interface{ Write([]byte) (int, error) }, resp *protocol.RegisterResponse) error {
	respData, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal registration response: %w", err)
	}

	ackFrame := protocol.NewFrame(protocol.FrameTypeRegisterAck, respData)
	return protocol.WriteFrame(conn, ackFrame)
}
