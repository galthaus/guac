package guac

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gitlab.com/rackn/logger"
)

const (
	readPrefix        string = "read:"
	writePrefix       string = "write:"
	readPrefixLength         = len(readPrefix)
	writePrefixLength        = len(writePrefix)
	uuidLength               = 36
)

// Server uses HTTP requests to talk to guacd (as opposed to WebSockets in ws_server.go)
type Server struct {
	logger  logger.Logger
	tunnels *TunnelMap
	connect func(*gin.Context) (Tunnel, error)
}

// NewServer constructor
func NewServer(l logger.Logger, connect func(*gin.Context) (Tunnel, error)) *Server {
	return &Server{
		logger:  l,
		tunnels: NewTunnelMap(),
		connect: connect,
	}
}

// Registers the given tunnel such that future read/write requests to that tunnel will be properly directed.
func (s *Server) registerTunnel(tunnel Tunnel) {
	s.tunnels.Put(tunnel.GetUUID(), tunnel)
	s.logger.Debugf("Registered tunnel %v.", tunnel.GetUUID())
}

// Deregisters the given tunnel such that future read/write requests to that tunnel will be rejected.
func (s *Server) deregisterTunnel(tunnel Tunnel) {
	s.tunnels.Remove(tunnel.GetUUID())
	s.logger.Debugf("Deregistered tunnel %v.", tunnel.GetUUID())
}

// Returns the tunnel with the given UUID.
func (s *Server) getTunnel(tunnelUUID string) (ret Tunnel, err error) {
	var ok bool
	ret, ok = s.tunnels.Get(tunnelUUID)

	if !ok {
		err = ErrResourceNotFound.NewError("No such tunnel.")
	}
	return
}

func (s *Server) sendError(response http.ResponseWriter, guacStatus Status, message string) {
	response.Header().Set("Guacamole-Status-Code", fmt.Sprintf("%v", guacStatus.GetGuacamoleStatusCode()))
	response.Header().Set("Guacamole-Error-Message", message)
	response.WriteHeader(guacStatus.GetHTTPStatusCode())
}

func (s *Server) ServeHTTP(c *gin.Context) {
	err := s.handleTunnelRequestCore(c)
	if err == nil {
		return
	}
	guacErr := err.(*ErrGuac)
	switch guacErr.Kind {
	case ErrClient:
		s.logger.Warnf("HTTP tunnel request rejected: %v", err.Error())
		s.sendError(c.Writer, guacErr.Status, err.Error())
	default:
		s.logger.Errorf("HTTP tunnel request failed: %v", err.Error())
		s.logger.Debugf("Internal error in HTTP tunnel: %v", err)
		s.sendError(c.Writer, guacErr.Status, "Internal server error.")
	}
	return
}

func (s *Server) handleTunnelRequestCore(c *gin.Context) (err error) {
	request := c.Request
	response := c.Writer

	query := request.URL.RawQuery
	if len(query) == 0 {
		return ErrClient.NewError("No query string provided.")
	}

	s.logger.Debugf("httptunnel: received: %s", query)

	// Call the supplied connect callback upon HTTP connect request
	if query == "connect" {
		s.logger.Debugf("httptunnel: Connecting to tunnel")
		tunnel, e := s.connect(c)
		if e != nil {
			err = ErrResourceNotFound.NewError("No tunnel created.", e.Error())
			return
		}
		s.logger.Debugf("httptunnel: Connected to tunnel")

		s.registerTunnel(tunnel)

		// Ensure buggy browsers do not cache response
		response.Header().Set("Cache-Control", "no-cache")
		response.Header().Set("Guacamole-Tunnel-Token", tunnel.GetUUID())

		if _, e := response.Write([]byte(tunnel.GetUUID())); e != nil {
			err = ErrServer.NewError(e.Error())
			return
		}

		s.logger.Debugf("httptunnel: write to client")
		return
	}

	// Connect has already been called so we use the UUID to do read and writes to the existing session
	if strings.HasPrefix(query, readPrefix) && len(query) >= readPrefixLength+uuidLength {
		err = s.doRead(c, query[readPrefixLength:readPrefixLength+uuidLength])
	} else if strings.HasPrefix(query, writePrefix) && len(query) >= writePrefixLength+uuidLength {
		err = s.doWrite(c, query[writePrefixLength:writePrefixLength+uuidLength])
	} else {
		err = ErrClient.NewError("Invalid tunnel operation: " + query)
	}

	return
}

// doRead takes guacd messages and sends them in the response
func (s *Server) doRead(c *gin.Context, tunnelUUID string) error {
	tunnel, err := s.getTunnel(tunnelUUID)
	if err != nil {
		return err
	}

	reader := tunnel.AcquireReader()
	defer tunnel.ReleaseReader()

	response := c.Writer

	// Note that although we are sending text, Webkit browsers will
	// buffer 1024 bytes before starting a normal stream if we use
	// anything but application/octet-stream.
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("Guacamole-Tunnel-Token", tunnel.GetUUID())

	if v, ok := response.(http.Flusher); ok {
		v.Flush()
	}

	err = s.writeSome(response, reader, tunnel)
	if err == nil {
		// success
		return nil
	}

	switch err.(*ErrGuac).Kind {
	// Send end-of-stream marker and close tunnel if connection is closed
	case ErrConnectionClosed:
		s.deregisterTunnel(tunnel)
		tunnel.Close()

		// End-of-instructions marker
		_, _ = response.Write([]byte("0.;"))
		if v, ok := response.(http.Flusher); ok {
			v.Flush()
		}
	default:
		s.logger.Debugf("Error writing to output: %v", err)
		s.deregisterTunnel(tunnel)
		tunnel.Close()
	}

	return err
}

// writeSome drains the guacd buffer holding instructions into the response
func (s *Server) writeSome(response http.ResponseWriter, guacd InstructionReader, tunnel Tunnel) (err error) {
	var message []byte

	for {
		message, err = guacd.ReadSome()
		if err != nil {
			s.deregisterTunnel(tunnel)
			tunnel.Close()
			return
		}

		if len(message) == 0 {
			return
		}

		_, e := response.Write(message)
		if e != nil {
			err = ErrOther.NewError(e.Error())
			return
		}

		if !guacd.Available() {
			if v, ok := response.(http.Flusher); ok {
				v.Flush()
			}
		}

		// No more messages another guacd can take over
		if tunnel.HasQueuedReaderThreads() {
			break
		}
	}

	// End-of-instructions marker
	if _, e := response.Write([]byte("0.;")); e != nil {
		return ErrOther.NewError(e.Error())
	}
	if v, ok := response.(http.Flusher); ok {
		v.Flush()
	}
	return nil
}

// doWrite takes data from the request and sends it to guacd
func (s *Server) doWrite(c *gin.Context, tunnelUUID string) error {
	tunnel, err := s.getTunnel(tunnelUUID)
	if err != nil {
		return err
	}

	// We still need to set the content type to avoid the default of
	// text/html, as such a content type would cause some browsers to
	// attempt to parse the result, even though the JavaScript client
	// does not explicitly request such parsing.
	c.Writer.Header().Set("Content-Type", "application/octet-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Guacamole-Tunnel-Token", tunnel.GetUUID())
	c.Writer.Header().Set("Content-Length", "0")

	writer := tunnel.AcquireWriter()
	defer tunnel.ReleaseWriter()

	_, err = io.Copy(writer, c.Request.Body)
	if err != nil {
		err = ErrOther.NewError(err.Error())
		s.deregisterTunnel(tunnel)
		if ec := tunnel.Close(); ec != nil {
			s.logger.Debugf("Error closing tunnel: %v", ec)
		}
	}

	// GREG: Does this need to close c.Request.Body

	return err
}
